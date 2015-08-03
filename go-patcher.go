package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexcesaro/log/stdlog"
)

const patcherURL = "https://admin.longsight.com/longsight/json/patches"
const patcherUserAgent = "GoPatcher v1.0"
const processGrepPattern = "ps x|grep -v grep|grep java"
const tomcatServerStartupPattern = "Server startup in"
const (
	patchSuccess     = "1"  // patchSuccess only when everything goes perfectly right
	tomcatDown       = "2"  // tomcatDown when tomcat never comes cleanly back
	tomcatNoShutdown = "4"  // tomcatNoShutdown when we can't kill Tomcat
	inProgress       = "10" // inProgress to block other patchers
)

var token = flag.String("token", "", "the custom security token")
var patchDir = flag.String("dir", "/tmp", "directory to store downloaded patches")
var patchWeb = flag.String("web", "https://s3.amazonaws.com/longsight-patches/", "website with patch files")
var localIP = flag.String("ip", "", "override automatic ip detection")
var startupWaitSeconds = flag.Int("waitTime", 240, "amount of time to wait for Tomcat to startup")
var propertyFiles = [4]string{"instance.properties", "dev.properties", "local.properties", "sakai.properties"}
var patcherUID = uint32(os.Getuid())
var logger = stdlog.GetFromFlags()
var outputBuffer bytes.Buffer

func init() {
	flag.Parse()
	if len(*token) < 1 {
		fmt.Println("Please provide a valid security token")
		os.Exit(1)
	}
}

func main() {
	ip, _ := externalIP()
	logger.Debug("Auto-detected IPs on this server:" + ip)

	// User is overriding the auto-detected IPs
	if len(*localIP) > 3 {
		ipJSON, err := json.Marshal(*localIP)
		if err != nil {
			panic("Bad ip provided on command-line")
		}
		ip = string(ipJSON)
		logger.Debug("User-overridden IP:", ip)
	}

	// See if there are any patches available for this IP
	data := checkForPatchesFromPortal(ip)

	// If no patches, exit nicely
	if len(data) < 1 {
		logger.Debug("No patches returned from portal")
		os.Exit(0)
	}

	// Extract info from the JSON patch info
	patchID := data["patch_id"].(string)
	tomcatDir := data["tomcat_dir"].(string)
	patchFiles := data["files"].(string)
	sakaiProperties := data["sakaiprops"].(string)
	logger.Debug("Patch returned from portal: ", data)

	// Make sure the Tomcat directory exists on this host
	checkTomcatDirExists(tomcatDir)
	checkTomcatOwnership(tomcatDir)

	// Update the admin portal to exclusively claim this patch
	updateAdminPortal(inProgress, "0", patchID)

	// Change working directory and stop Tomcat
	os.Chdir(tomcatDir)
	logger.Debug("Chdir to ", tomcatDir)
	stopTomcat(tomcatDir)

	// Modify the properties files
	if len(sakaiProperties) > 0 {
		modifyPropertyFiles(sakaiProperties, patchID)
	}

	// Unroll the tarball
	if len(patchFiles) > 3 {
		if strings.Contains(patchFiles, " ") {
			patches := strings.SplitN(patchFiles, " ", 10)
			for _, patch := range patches {
				applyTarballPatch(patch)
			}
		} else {
			applyTarballPatch(patchFiles)
		}
	}

	// Time to start up Tomcat
	startTomcat(tomcatDir, patchID)

	// Check for server startup in logs/catalina.out
	z := 42
	for z < *startupWaitSeconds {
		serverStartupTime := checkServerStartup()
		if !strings.Contains(serverStartupTime, "false") {
			parsedTime := parseServerStartupTime(serverStartupTime)
			if parsedTime > 0 {
				updateAdminPortal(patchSuccess, strconv.FormatInt(parsedTime, 10), patchID)
			} else {
				updateAdminPortal(tomcatDown, "-1", patchID)
			}

			// Exiting after patching!
			os.Exit(0)
		}
		time.Sleep(10 * 1000 * time.Millisecond)
		z += 10
		logger.Debug("Checking logs again in 10 seconds")
	}

	// Couldn't find success in Tomcat logs
	updateAdminPortal(tomcatDown, "-1", patchID)
}

func parseServerStartupTime(logLine string) int64 {
	p := strings.SplitN(string(logLine), " ", 10)
	for _, ps := range p {
		k, err := strconv.Atoi(ps)
		if err == nil && k > 1000 {
			logger.Debug("Found 'Server startup' in Tomcat catalina.out: ", k)
			return int64(k)
		}
	}

	return -1
}

func updateAdminPortal(rv string, startup string, patchID string) {
	// Grab the text from the Tomcat startup and shutdown
	resultText := outputBuffer.String()

	// Unix time converted to a string
	currentTime := strconv.FormatInt(time.Now().Unix(), 10)

	postURL := "https://admin.longsight.com/longsight/remote/patch/update"
	urlValues := url.Values{"result_value": {rv}, "start_uptime": {startup},
		"last_attempt": {string(currentTime)}, "patch_id": {patchID}, "result": {resultText}}
	logger.Debug("Values being sent to admin portal: ", urlValues)

	resp, err := http.PostForm(postURL, urlValues)
	logger.Debug("Response from admin portal: ", resp)

	if err != nil {
		panic("Could not POST update")
	}
}

func checkServerStartup() string {
	file, err := os.Open("logs/catalina.out")
	if err != nil {
		panic("Could not open logs/catalina.out")
	}

	defer file.Close()

	reader := bufio.NewReader(file)
	scanner := bufio.NewScanner(reader)
	scanner.Split(bufio.ScanLines)

	linesScanned := 0
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), tomcatServerStartupPattern) {
			return scanner.Text()
		}
		linesScanned++
	}

	logger.Debug("Scanned lines in logs/catalina.out", linesScanned)

	return "false"
}

func startTomcat(tomcatDir string, patchID string) {
	// Move the old catalina.out so we can look for the ServerStatup cleanly
	os.Rename("logs/catalina.out", "logs/catalina.out-pre-patch-"+patchID)

	out, _ := exec.Command("bin/catalina.sh", "start").CombinedOutput()
	logger.Debug("startTomcat: ", string(out))
	outputBuffer.Write(out)
}

func stopTomcat(tomcatDir string) {
	out, err := exec.Command("bin/catalina.sh", "stop").CombinedOutput()
	if err != nil {
		logger.Warning("Error when shutting down Tomcat: ", err)
	}
	logger.Debug("stopTomcat: ", string(out))

	time.Sleep(10 * 1000 * time.Millisecond)
	hardKillProcess(tomcatDir)
	time.Sleep(10 * 1000 * time.Millisecond)
	hardKillProcess(tomcatDir)
	outputBuffer.Write(out)
}

func fetchTarball(tarball string) string {
	fullPath := tarball
	fileName := path.Base(tarball)
	logger.Debug("fetchTarball: ", fileName, fullPath)

	// See if the file exists in local patch directory
	if !pathExists(fullPath) {
		fullPath = *patchDir + string(os.PathSeparator) + fileName
		logger.Debug("fetchTarball new path to try: ", fullPath)
	}

	// See if we can pull file from S3
	if !pathExists(fullPath) {
		fileWriter, err := os.Create(fullPath)
		if err != nil {
			panic("Could not open file: " + fullPath)
		}
		defer fileWriter.Close()

		resp, err := http.Get(*patchWeb + "sakai-builder/" + fileName)
		if err != nil {
			logger.Warning("Could not fetch patch: " + *patchWeb + "sakai-builder/" + fileName)
		} else {
			logger.Debug("Trying to fetch patch: ", *patchWeb+"sakai-builder/"+fileName, resp)
		}

		if resp.StatusCode != http.StatusOK {
			resp, err = http.Get(*patchWeb + "patches/" + fileName)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			n, err := io.Copy(fileWriter, resp.Body)
			logger.Debug("Copied remote file bytes: ", n)
			if n > 0 && err != nil {
				panic("Could not copy from web to local file system")
			}
		}
	}

	if !pathExists(fullPath) {
		panic("Could not find the patch file: " + fileName)
	}

	logger.Debug("Final patch path: " + fullPath)
	return fullPath
}

func applyTarballPatch(tarball string) {
	filePath := fetchTarball(tarball)

	// Unroll the tarball one time to see what to clean out
	fileMap := unrollTarball(filePath)

	// Clean out old directories
	for fileMapPath, cnt := range fileMap {
		isWebapp := strings.HasPrefix(fileMapPath, "webapps")
		isWarFile := strings.HasSuffix(fileMapPath, ".war")
		isComponents := strings.HasPrefix(fileMapPath, "components")
		isSharedJar := isLibJar(fileMapPath)
		isProvidersDir := strings.Contains(fileMapPath, "sakai-provider-pack")
		pathArray := strings.Split(fileMapPath, "/")
		pathToDelete := pathArray[0] + "/" + pathArray[1]

		if cnt > 3 && isComponents && !isProvidersDir {
			err := os.RemoveAll(pathToDelete)
			if err != nil {
				panic("Could not remove components path: " + pathToDelete)
			}
			logger.Debug("Deleting components path: ", pathToDelete)
		} else if isWebapp && isWarFile {
			webappFolder := strings.TrimRight(pathToDelete, ".war")
			err := os.RemoveAll(webappFolder)
			if err != nil {
				panic("Could not remove webapp path: " + webappFolder)
			}
			logger.Debug("Deleting webapp path: ", webappFolder)
		} else if isSharedJar {
			// Need to wildcard the name to remove old versions
			wildcardedFilename := replaceNumbers(fileMapPath)
			wildcardedFilename = strings.Replace(wildcardedFilename, "-SNAPSHOT", "", 1)
			err := os.RemoveAll(wildcardedFilename)
			if err != nil {
				panic("Could not delete wildcarded path: " + wildcardedFilename)
			}
			logger.Debug("Delete shared JAR: ", wildcardedFilename)
		}
	}

	// Unroll the tarball again after cleaning out old directories
	unrollTarball(filePath)
}

func isLibJar(filename string) bool {
	isSharedJar := strings.HasPrefix(filename, "shared/lib")
	isCommonJar := strings.HasPrefix(filename, "common/lib")
	isJarFile := strings.HasSuffix(filename, ".jar")
	return (isSharedJar || isCommonJar) && isJarFile
}

func unrollTarball(filePath string) map[string]int {
	var m map[string]int
	m = make(map[string]int)

	file, err := os.Open(filePath)

	if err != nil {
		panic("Could not open patch: " + filePath)
	}

	defer file.Close()

	var fileReader io.ReadCloser = file

	if strings.HasSuffix(filePath, ".gz") {
		if fileReader, err = gzip.NewReader(file); err != nil {
			panic("Could not read GZIP")
		}
		defer fileReader.Close()
	}

	tarBallReader := tar.NewReader(fileReader)
	for {
		header, err := tarBallReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			panic("Could not read tarball")
		}

		// get the individual filename and extract to the current directory
		filename := header.Name

		switch header.Typeflag {
		case tar.TypeDir:
			// handle directory
			err = os.MkdirAll(filename, os.FileMode(header.Mode)) // or use 0755 if you prefer
			logger.Debug("Creating directory: ", filename)

			if err != nil {
				panic("Could not create directory: " + filename)
			}

		case tar.TypeReg:
			// handle normal file

			// Strip files that start with dot slash
			startsWithDot := strings.HasPrefix(filename, "./")
			if startsWithDot {
				filename = strings.Replace(filename, "./", "", 1)
				logger.Debug("Tar file started with a dot: ", filename)
			}

			// See if there are any dirs we should wipe out
			if len(filename) > len("components/a") {
				splitPaths := strings.Split(filename, "/")
				if len(splitPaths) > 1 {
					firstTwoPaths := splitPaths[0] + "/" + splitPaths[1]

					_, ok := m[firstTwoPaths]
					if isLibJar(filename) {
						m[filename] = 1
					} else if ok {
						m[firstTwoPaths]++
					} else {
						m[firstTwoPaths] = 1
					}
				}
			}

			// Skip jldap-beans.xml
			if strings.Contains(filename, "jldap-bean") || strings.Contains(filename, "sakai-provider-pack/WEB-INF/components.xml") {
				logger.Debug("Skipping JLDAP file:", filename)
				continue
			}

			writer, err := os.Create(filename)
			logger.Debug("Unrolled tarball file: ", filename)

			if err != nil {
				logger.Error("Could not create file from tarball: ", filename, err)
			}

			io.Copy(writer, tarBallReader)

			err = os.Chmod(filename, os.FileMode(header.Mode))

			if err != nil {
				logger.Error("Could not chmod file: ", filename, err)
			}

			writer.Close()
		default:
			logger.Errorf("Unable to untar type : %c in file %s", header.Typeflag, filename)
		}
	}

	return m
}

func checkForProcess(tomcatDir string) bool {
	out, _ := exec.Command("bash", "-c", processGrepPattern+"|grep "+tomcatDir).Output()
	numLines := strings.Split(string(out), "\n")
	logger.Debug("Process lines:", len(numLines))
	if len(numLines) > 2 {
		panic("Number of processes: " + string(out))
	}
	logger.Debug("Checking for process: ", string(out))
	if len(out) > 0 {
		return true
	}

	return false
}

func hardKillProcess(tomcatDir string) {
	alive := checkForProcess(tomcatDir)

	if alive {
		out, _ := exec.Command("bash", "-c", processGrepPattern+"|grep "+tomcatDir).Output()
		p := strings.SplitN(string(out), " ", 3)
		for _, ps := range p {
			k, err := strconv.Atoi(ps)
			if err == nil && k > 100 {
				logger.Debug("Hard killing process: ", k)
				syscall.Kill(k, 9)
			}
		}
	}
}

func checkForPatchesFromPortal(ip string) map[string]interface{} {
	data := map[string]interface{}{}
	url := patcherURL + "?ips=" + ip

	req, err := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Auth-Token", *token)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("User-Agent", patcherUserAgent)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)

		// We have a real patch
		if len(body) > 5 {
			json.Unmarshal(body, &data)
			logger.Debug("Raw data from admin portal: ", data)
		}
	} else {
		logger.Alertf("Bad HTTP fetch: %v \n", resp.Status)
		os.Exit(1)
	}

	return data
}

func checkTomcatDirExists(tomcatDir string) {
	tomcatExists := pathExists(tomcatDir)
	if !tomcatExists {
		panic("TomcatDir does not exist: " + tomcatDir)
	}
}

func checkTomcatOwnership(tomcatDir string) {
	file, err := os.Open(tomcatDir + "/bin/catalina.sh")
	if err != nil {
		panic("Could not open file: " + tomcatDir + "/bin/catalina.sh")
	}
	fi, _ := file.Stat()
	tomcatUID := fi.Sys().(*syscall.Stat_t).Uid
	logger.Debug("Tomcat ownership uid: ", tomcatUID)
	if tomcatUID != patcherUID {
		logger.Warning("Patcher UID is different from Tomcat UID", tomcatUID, patcherUID)
		os.Exit(1)
	}
}

func externalIP() (string, error) {
	var ips [5]string
	counter := 0

	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			ips[counter] = ip.String()
			counter++
		}
	}

	// Conver the IP array into a JSON string
	b, err := json.Marshal(ips)
	if err != nil {
		panic(err)
	}

	return string(b), errors.New("Are you connected to the network?")
}

func modifyPropertyFiles(rawProperties string, patchID string) {
	newProperties := strings.Split(rawProperties, "\n")

	// Loop through every property we are patching
	for _, newPropertyLine := range newProperties {
		newPropertyKey := strings.TrimRight(newPropertyLine, "=")
		logger.Debug("New property key=" + newPropertyKey)
		addedTheNewProperty := false

		// Loop through all known property file names
		for _, propertyFile := range propertyFiles {
			fileModified := false
			propertyFilePath := "sakai/" + propertyFile
			if pathExists(propertyFilePath) {
				logger.Debug("Found property file: " + propertyFilePath)
				input, err := ioutil.ReadFile(propertyFilePath)
				if err != nil {
					logger.Error("Could not open property file: " + propertyFilePath)
				}

				lines := strings.Split(string(input), "\n")
				for i, line := range lines {
					if strings.Contains(line, newPropertyKey) {
						if !strings.Contains(line, "#"+newPropertyKey) {
							logger.Debug("Found property key: " + line)
							lines[i] = "#" + line
							fileModified = true
						}
					}
				}

				output := strings.Join(lines, "\n")
				if !addedTheNewProperty {
					output += "\n# Longsight patch ID: " + patchID
					output += "\n" + newPropertyLine
					logger.Debug("Added new line to file: "+propertyFilePath, newPropertyLine)
					fileModified = true
					addedTheNewProperty = true
				}

				if fileModified {
					err = ioutil.WriteFile(propertyFilePath, []byte(output), 0644)
					if err != nil {
						logger.Error("Could not write revised file: " + propertyFilePath)
					}
				}
			}
		}
	}
}

func replaceNumbers(s string) string {
	out := make([]rune, len(s)) // len(s) is bytes not runes, this is just estimation

	i, added := 0, false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			if added {
				continue
			}
			added, out[i] = true, '*'
		} else {
			added, out[i] = false, r
		}
		i++
	}
	return string(out[:i])
}

// exists returns whether the given file or directory exists or not
func pathExists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	return true
}
