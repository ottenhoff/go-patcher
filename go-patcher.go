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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
)

const patcherURL = "https://admin.longsight.com/longsight/json/patches"
const patcherUserAgent = "GoPatcher v1.0"
const processGrepPattern = "ps x|grep -v grep|grep java"
const tomcatServerStartupPattern = "Server startup in"
const igniteMismatchPattern = "Fix cache configuration or set system property"
const legacyPatchDir = "/patches/"
const (
	patchDefer       = "0"  // try again in a bit
	patchSuccess     = "1"  // patchSuccess only when everything goes perfectly right
	tomcatDown       = "2"  // tomcatDown when tomcat never comes cleanly back
	tomcatNoShutdown = "4"  // tomcatNoShutdown when we can't kill Tomcat
	inProgress       = "10" // inProgress to block other patchers
)

// Declare flag variables as global variables
var token *string
var logLevel *string
var patchDir *string
var patchWeb *string
var localIP *string
var startupWaitSeconds *int

var propertyFiles = [4]string{"sakai.properties", "dev.properties", "local.properties", "instance.properties"}
var skipPattern = regexp.MustCompile(`^components/sakai-provider-pack/WEB-INF/.*(unboundid|components|jldap).*\.xml$`)
var patcherUID = uint32(os.Getuid())
var outputBuffer bytes.Buffer

func main() {
	initParseCommandLineFlags()

	ip, _ := externalIP()
	log.Debug("Auto-detected IPs on this server:" + ip)

	// User is overriding the auto-detected IPs
	if len(*localIP) > 3 {
		var ipArray = [1]string{*localIP}
		ipJSON, err := json.Marshal(ipArray)
		if err != nil {
			panic("Bad ip provided on command-line")
		}
		ip = string(ipJSON)
		log.Debug("User-overridden IP:", ip)
	}

	// See if there are any patches available for this IP
	data := checkForPatchesFromPortal(ip)

	// If no patches, exit nicely
	if len(data) < 1 {
		log.Debug("No patches returned from portal")
		os.Exit(0)
	}

	// Extract info from the JSON patch info
	patchID := data["patch_id"].(string)
	tomcatDir := data["tomcat_dir"].(string)
	patchFiles := data["files"].(string)
	sakaiProperties := data["sakaiprops"].(string)
	log.Debug("Patch returned from portal: ", data)

	// Make sure the Tomcat directory exists on this host
	checkTomcatDirExists(tomcatDir)
	checkTomcatOwnership(tomcatDir)

	// Update the admin portal to exclusively claim this patch
	updateAdminPortal(inProgress, "0", patchID)

	os.Chdir(tomcatDir)
	log.Debug("Chdir to ", tomcatDir)
	stopTomcat(tomcatDir)

	// Modify the properties files
	if len(sakaiProperties) > 0 {
		// Kill Tomcat and exit for special scenario
		if strings.TrimSpace(sakaiProperties) == "die" {
			log.Errorf("Killing Tomcat per patcher: %s", tomcatDir)
			updateAdminPortal(patchSuccess, "1", patchID)
			os.Exit(0)
		} else {
			modifyPropertyFiles(sakaiProperties, patchID)
		}
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

		// Update the version to better cache bust
		// We are going to save bytes and just use the last two digits of the patch ID
		modifyPropertyFiles("portal.cdn.version="+patchID[len(patchID)-3:], patchID)
	}

	// Clean up the lib so we don't have dupe mysql-connector JARs
	checkForUnnecessaryJars(tomcatDir)

	// Time to start up Tomcat
	startTomcat(patchID)

	// Check for server startup in logs/catalina.out after 40 seconds
	time.Sleep(40 * 1000 * time.Millisecond)
	for z := 40; z < *startupWaitSeconds; z += 10 {
		serverStartupTime := checkServerStartup()
		if strings.Contains(serverStartupTime, "ignite") {
			log.Warning("Found ignite error in logs. Will try again later.")
			updateAdminPortal(patchDefer, "-2", patchID)
			os.Exit(0)
		} else if !strings.Contains(serverStartupTime, "false") {
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
		log.Debug("Checking logs again. Seconds elapsed:", z)
	}

	// Couldn't find success in Tomcat logs
	updateAdminPortal(tomcatDown, "-1", patchID)
	os.Exit(0)
}

func parseServerStartupTime(logLine string) int64 {
	// Tomcat 8.5+
	if strings.Contains(logLine, "milliseconds") {
		beginBracket := strings.LastIndex(logLine, "[") + 1
		endComma := strings.LastIndex(logLine, "]")
		substr := strings.Replace(logLine[beginBracket:endComma], ",", "", -1)
		k, err := strconv.Atoi(substr)
		if err == nil && k > 1000 {
			log.Debug("Found 'Server startup' in Tomcat 8.5+ catalina.out: ", k)
			return int64(k)
		}
	}

	// Older Tomcats
	p := strings.SplitN(string(logLine), " ", 10)
	for _, ps := range p {
		k, err := strconv.Atoi(ps)
		if err == nil && k > 1000 {
			log.Debug("Found 'Server startup' in Tomcat catalina.out: ", k)
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
	log.Debug("Values being sent to admin portal: ", urlValues)

	resp, err := http.PostForm(postURL, urlValues)
	log.Debug("Response from admin portal: ", resp)

	if err != nil {
		panic("Could not POST update")
	}
}

func checkForUnnecessaryJars(tomcatDir string) {
	catalinaHome := ""

	file, err := os.Open("bin/setenv.sh")
	if err != nil {
		panic("Could not open bin/setenv.sh")
	}

	defer file.Close()

	reader := bufio.NewReader(file)

	// Find the Tomcat home in the environment vars.
	for {
		line, err := reader.ReadString('\n')

		if strings.Contains(line, "CATALINA_HOME") {
			pathArray := strings.Split(line, "=")
			catalinaHome = pathArray[1]
			break
		}

		if err == io.EOF {
			break
		}
	}

	if catalinaHome != "" {
		catalinaHome = strings.Trim(catalinaHome, "\" \n\"") + "/lib"
		centralFiles, err := os.ReadDir(catalinaHome)
		if err != nil {
			log.Error("Could not read catalinaHome", err)
			return
		}

		tomcatFiles, err := os.ReadDir(tomcatDir + "/lib")
		if err != nil {
			log.Error("Could not read tomcatDir", err)
			return
		}

		// Look for old ignite/hibernate JAR
		oldIgniteHibernateJar := ""
		oldIgniteHibernateCoreJar := ""
		oldCommonsTextJar := ""
		oldJaxbJar := ""
		oldHttpCoreJar := ""
		for _, tomcatFile := range tomcatFiles {
			if strings.Contains(tomcatFile.Name(), "ignite-hibernate_5.3-") {
				oldIgniteHibernateJar = tomcatFile.Name()
			} else if strings.Contains(tomcatFile.Name(), "ignite-hibernate-core-") {
				oldIgniteHibernateCoreJar = tomcatFile.Name()
			} else if strings.Contains(tomcatFile.Name(), "commons-text-1.9.jar") {
				oldCommonsTextJar = tomcatFile.Name()
			} else if strings.Contains(tomcatFile.Name(), "jaxb-impl-2") {
				oldJaxbJar = tomcatFile.Name()
			} else if strings.Contains(tomcatFile.Name(), "httpcore5-5.2.jar") {
				oldHttpCoreJar = tomcatFile.Name()
			}
		}

		for _, tomcatFile := range tomcatFiles {
			for _, centralFile := range centralFiles {
				// If the file is in central Tomcat lib, no need for it here.
				if tomcatFile.Name() == centralFile.Name() {
					os.Remove(tomcatDir + "/lib/" + tomcatFile.Name())
					log.Debug("Removed " + tomcatDir + "/lib/" + tomcatFile.Name())
				} else if strings.Contains(centralFile.Name(), "mysql-connector") && strings.Contains(tomcatFile.Name(), "mysql-connector") {
					os.Remove(tomcatDir + "/lib/" + tomcatFile.Name())
					log.Debug("Removed " + tomcatDir + "/lib/" + tomcatFile.Name())
				}
			}
			// We dont use mariadb connector or terracotta
			if strings.Contains(tomcatFile.Name(), "mariadb") || strings.Contains(tomcatFile.Name(), "terracotta") || strings.Contains(tomcatFile.Name(), "hazelcast") {
				os.Remove(tomcatDir + "/lib/" + tomcatFile.Name())
				log.Debug("Removed " + tomcatDir + "/lib/" + tomcatFile.Name())
				// Ignite/Hibernate modified some JARs mid-22.x
			} else if strings.Contains(tomcatFile.Name(), "ignite-hibernate-ext-5.3") {
				if oldIgniteHibernateJar != "" {
					os.Remove(tomcatDir + "/lib/" + oldIgniteHibernateJar)
					log.Info("Removed " + tomcatDir + "/lib/" + oldIgniteHibernateJar + " because new ignite-hibernate-ext")
				}
				if oldIgniteHibernateCoreJar != "" {
					os.Remove(tomcatDir + "/lib/" + oldIgniteHibernateCoreJar)
					log.Info("Removed " + tomcatDir + "/lib/" + oldIgniteHibernateCoreJar + " because new ignite-hibernate-ext")
				}
			} else if strings.Contains(tomcatFile.Name(), "commons-text-1.10.") && oldCommonsTextJar != "" {
				os.Remove(tomcatDir + "/lib/" + oldCommonsTextJar)
				log.Info("Removed " + tomcatDir + "/lib/" + oldCommonsTextJar)
			} else if strings.Contains(tomcatFile.Name(), "jaxb-runtime-2.3.6.jar") && oldJaxbJar != "" {
				os.Remove(tomcatDir + "/lib/" + oldJaxbJar)
				log.Info("Removed " + tomcatDir + "/lib/" + oldJaxbJar)
			} else if strings.Contains(tomcatFile.Name(), "httpcore5-5.2.3.jar") && oldHttpCoreJar != "" {
				os.Remove(tomcatDir + "/lib/" + oldHttpCoreJar)
				log.Info("Removed " + tomcatDir + "/lib/" + oldHttpCoreJar)
			}
		}
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
		if strings.Contains(scanner.Text(), igniteMismatchPattern) {
			return "ignite"
		}
		if strings.Contains(scanner.Text(), tomcatServerStartupPattern) {
			return scanner.Text()
		}
		linesScanned++
	}

	log.Debug("Scanned lines in logs/catalina.out", linesScanned)

	return "false"
}

func startTomcat(patchID string) {
	// Move the old catalina.out so we can look for the ServerStatup cleanly
	// TODO: gzip the old log file
	os.Rename("logs/catalina.out", "logs/catalina.out-pre-patch-"+patchID)

	out, _ := exec.Command("bin/catalina.sh", "start").CombinedOutput()
	log.Debug("startTomcat: ", string(out))
	outputBuffer.Write(out)
}

func stopTomcat(tomcatDir string) {
	out, err := exec.Command("bin/catalina.sh", "stop", "32", "-force").CombinedOutput()
	if err != nil {
		log.Warning("Error when shutting down Tomcat: ", err)
	}
	log.Debug("stopTomcat: ", string(out))

	time.Sleep(20 * 1000 * time.Millisecond)
	hardKillProcess(tomcatDir)
	time.Sleep(10 * 1000 * time.Millisecond)
	hardKillProcess(tomcatDir)
	outputBuffer.Write(out)
}

func fetchTarball(tarball string) string {
	fullPath := tarball
	fileName := path.Base(tarball)
	log.Debug("fetchTarball: ", fileName, fullPath)

	// See if the file exists in local patch directory
	if !pathExists(fullPath) {
		fullPath = *patchDir + string(os.PathSeparator) + fileName

		// Delete old file in our tmp dir
		if pathExists(fullPath) {
			os.Remove(fullPath)
			log.Debug("Deleted old temp file: ", fullPath)
		}
		log.Debug("fetchTarball new path to try: ", fullPath)
	}

	// See if we can pull file from S3
	if !pathExists(fullPath) {
		fileWriter, err := os.Create(fullPath)
		if err != nil {
			panic("Could not open file: " + fullPath)
		}
		defer fileWriter.Close()

		// Try to correct the path
		fileToFetch := *patchWeb + "sakai-builder/" + fileName
		if strings.Contains(tarball, legacyPatchDir) {
			fileToFetch = *patchWeb + strings.Replace(tarball, legacyPatchDir, "patches/", 1)
		}

		resp, err := http.Get(fileToFetch)
		log.Debug("Trying to fetch patch: " + fileToFetch)
		if err != nil {
			panic("Could not download patch")
		}
		contentLength := resp.Header.Get("Content-Length")
		if contentLength == "" {
			log.Warn("No Content-Length header found on download")
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			n, err := io.Copy(fileWriter, resp.Body)
			log.Debug("Copied remote file bytes: ", n)

			if n > 0 && err != nil {
				os.Remove(fullPath)
				panic("Could not copy from web to local file system")
			} else if n > 0 && contentLength != "" {
				expectedSize, err := strconv.ParseInt(contentLength, 10, 64)
				if err != nil {
					log.Error("Error parsing Content-Length:", err)
				} else if n != expectedSize {
					panic("Downloaded bytes (" + strconv.FormatInt(n, 10) + ") is different from Content-Length: " + contentLength)
				}
			}
		} else {
			log.Error("Could not find patch.... proceeding", resp)
		}
	}

	if !pathExists(fullPath) {
		panic("Could not find the patch file: " + fileName)
	}

	log.Debug("Final patch path: " + fullPath)
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
			log.Debug("Deleting components path: ", pathToDelete)

			// Special case with content-review
			if strings.Contains(pathToDelete, "sakai-content-review-pack-federated") {
				err := os.RemoveAll("components/sakai-content-review-pack")
				if err != nil {
					panic("Could not remove special path: components/sakai-content-review-pack")
				}
				log.Debug("Special path delete: components/sakai-content-review-pack")
			}
		} else if isWebapp && isWarFile {
			webappFolder := trimSuffix(pathToDelete, ".war")
			err := os.RemoveAll(webappFolder)
			if err != nil {
				panic("Could not remove webapp path: " + webappFolder)
			}
			log.Debug("Deleting webapp path: ", webappFolder)
		} else if isSharedJar {
			// Need to wildcard the name to remove old versions
			wildcardedFilename := replaceNumbers(fileMapPath)
			if strings.Contains(fileMapPath, "gradebook2") {
				wildcardedFilename = fileMapPath
			}
			wildcardedFilename = strings.Replace(wildcardedFilename, "-SNAPSHOT", "", 1)
			err := removeFiles(wildcardedFilename)
			if err != nil {
				panic("Could not delete wildcarded path: " + wildcardedFilename)
			}
		}
	}

	// Unroll the tarball again after cleaning out old directories
	unrollTarball(filePath)
}

func isLibJar(filename string) bool {
	isSharedJar := strings.HasPrefix(filename, "shared/lib/")
	isCommonJar := strings.HasPrefix(filename, "common/lib/")
	isLibDirJar := strings.HasPrefix(filename, "lib/")
	isJarFile := strings.HasSuffix(filename, ".jar")
	return (isSharedJar || isCommonJar || isLibDirJar) && isJarFile
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

	if strings.HasSuffix(filePath, ".zst") {
		decoder, err := zstd.NewReader(file)
		if err != nil {
			panic("Could not create zstd reader")
		}
		defer decoder.Close()
		fileReader = io.NopCloser(decoder)
	} else if strings.HasSuffix(filePath, ".gz") {
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
			if !pathExists(filename) {
				err = os.MkdirAll(filename, os.FileMode(header.Mode)) // or use 0755 if you prefer
				log.Debug("Creating directory: ", filename)

				if err != nil {
					panic("Could not create directory: " + filename)
				}
			}

		case tar.TypeReg:
			// handle normal file

			// Strip files that start with dot slash
			startsWithDot := strings.HasPrefix(filename, "./")
			if startsWithDot {
				filename = strings.Replace(filename, "./", "", 1)
				log.Debug("Tar file started with a dot: ", filename)
			}

			// Do not overwrite an existing jldap-beans.xml or unboundid-ldap.xml or components.xml
			if shouldSkipFile(filename) && pathExists(filename) {
				log.Debug("Skipping file: ", filename)
				continue
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

			writer, err := os.Create(filename)
			log.Debug("Unrolled tarball file: ", filename)

			if err != nil {
				log.Error("Could not create file from tarball: ", filename, err)
			}

			io.Copy(writer, tarBallReader)

			err = os.Chmod(filename, os.FileMode(header.Mode))

			if err != nil {
				log.Error("Could not chmod file: ", filename, err)
			}

			writer.Close()
		default:
			log.Errorf("Unable to untar type : %c in file %s", header.Typeflag, filename)
		}
	}

	return m
}

func shouldSkipFile(filename string) bool {
	return skipPattern.MatchString(filename)
}

func checkForProcess(tomcatDir string) bool {
	out, _ := exec.Command("bash", "-c", processGrepPattern+"|grep "+tomcatDir).Output()
	numLines := strings.Split(string(out), "\n")
	log.Debug("Process lines:", len(numLines))
	if len(numLines) > 2 {
		panic("Number of processes: " + string(out))
	}
	log.Debug("Checking for process: ", string(out))
	if len(numLines) > 0 {
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
				log.Debug("Hard killing process: ", k)
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
		body, _ := io.ReadAll(resp.Body)

		// We have a real patch
		if len(body) > 5 {
			json.Unmarshal(body, &data)
			log.Debug("Raw data from admin portal: ", data)
		}
	} else {
		log.Errorf("Bad HTTP fetch: %v \n", resp.Status)
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
	log.Debug("Tomcat ownership uid: ", tomcatUID)
	if tomcatUID != patcherUID {
		log.Debug("Patcher UID is different from Tomcat UID", tomcatUID, patcherUID)
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
		addrs, addrerr := iface.Addrs()
		if addrerr != nil {
			return "", addrerr
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
		newPropertyKey := "defaultkeyvalueimpossibletofind"
		if strings.Contains(newPropertyLine, "=") && !strings.Contains(newPropertyLine, "#") {
			newPropertyArray := strings.Split(newPropertyLine, "=")
			newPropertyKey = newPropertyArray[0]
			log.Debug("New property key=" + newPropertyKey)
		}
		addedTheNewProperty := false

		// Loop through all known property file names
		lastValidPropertyFile := ""
		for _, propertyFile := range propertyFiles {
			fileModified := false
			propertyFilePath := "sakai/" + propertyFile
			if pathExists(propertyFilePath) {
				log.Debug("Found property file: " + propertyFilePath)
				input, err := os.ReadFile(propertyFilePath)
				if err != nil {
					log.Error("Could not open property file: " + propertyFilePath)
					continue
				}
				lastValidPropertyFile = propertyFilePath

				lines := strings.Split(string(input), "\n")
				for i, line := range lines {
					if strings.Contains(line, newPropertyKey) {
						if !strings.Contains(line, "#"+newPropertyKey) {
							log.Debug("Found property key: " + line)
							lines[i] = "#" + line
							lines = append(lines[:i+1], append([]string{newPropertyLine}, lines[i+1:]...)...)
							fileModified = true
							addedTheNewProperty = true
						}
					}
				}

				if fileModified {
					output := strings.Join(lines, "\n")
					err = os.WriteFile(propertyFilePath, []byte(output), 0644)
					if err != nil {
						log.Error("Could not write revised file: " + propertyFilePath)
					}
				}
			}
		}

		// This is a brand-new property that we need to add to the last valid property file
		if !addedTheNewProperty && lastValidPropertyFile != "" {
			input, err := os.ReadFile(lastValidPropertyFile)
			if err != nil {
				log.Error("Could not open property file for new property: " + lastValidPropertyFile)
				continue
			}

			lines := strings.Split(string(input), "\n")
			lines = append(lines, "# Longsight patch ID: "+patchID+" ("+time.Now().Format("2006-01-02 15:04:05")+")")
			lines = append(lines, newPropertyLine)

			output := strings.Join(lines, "\n")
			err = os.WriteFile(lastValidPropertyFile, []byte(output), 0644)
			if err != nil {
				log.Error("Could not write new property to file: " + lastValidPropertyFile)
			} else {
				log.Debug("Added new line to file: "+lastValidPropertyFile, newPropertyLine)
			}
		}
	}
}

// replaceNumbers replaces consecutive digits in a string with a single asterisk.
func replaceNumbers(s string) string {
	// Initialize an output slice of runes to store the transformed characters.
	// The length is estimated based on the input string length.
	outputRunes := make([]rune, 0, len(s))

	// Track whether the last rune was a digit to handle consecutive digits.
	lastWasDigit := false

	// Iterate over each rune in the input string.
	for _, currentRune := range s {
		// Check if the current rune is a digit.
		if currentRune >= '0' && currentRune <= '9' {
			// If the last rune was not a digit, add an asterisk to the output.
			if !lastWasDigit {
				outputRunes = append(outputRunes, '*')
				lastWasDigit = true
			}
			// Skip adding the digit itself.
			continue
		}
		// For non-digit runes, add them to the output and set lastWasDigit to false.
		outputRunes = append(outputRunes, currentRune)
		lastWasDigit = false
	}
	// Convert the output runes back to a string and return it.
	return string(outputRunes)
}

// exists returns whether the given file or directory exists or not
func pathExists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true // The file exists
	}
	if os.IsNotExist(err) {
		return false // The file does not exist
	}
	// Log other types of errors
	log.Printf("Error checking if path exists: %v\n", err)
	return false // Treat other errors as if the file does not exist
}

// Shortcut to check if the path is a directory
func isDir(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}

func removeFiles(wildcardedPath string) error {
	files, err := filepath.Glob(wildcardedPath)
	if err != nil {
		log.Errorf("Failed to glob %s", wildcardedPath)
		return err
	}
	log.Debugf("Found files matching %s: %v", wildcardedPath, files)

	toSkip := make(map[string]bool)
	for _, file := range files {
		dir, derr := isDir(file)
		if derr != nil {
			return derr
		}
		if dir {
			toSkip[file] = true
		} else {
			realFile, symerr := filepath.EvalSymlinks(file)
			if symerr != nil {
				log.Error("Failed to eval symlink", file, symerr)
				return symerr
			}
			if realFile != file {
				toSkip[file] = true
				toSkip[realFile] = true
			}
		}
	}

	for _, file := range files {
		if toSkip[file] {
			log.Debugf("Skipping file: %s", file)
		} else {
			log.Debugf("Removing: %s", file)
			err = os.Remove(file)
			if err != nil {
				log.Errorf("Failed to remove %s: %s", file, err)
				return err
			}
		}
	}
	return nil
}

func trimSuffix(s, suffix string) string {
	if strings.HasSuffix(s, suffix) {
		s = s[:len(s)-len(suffix)]
	}
	return s
}

func initParseCommandLineFlags() {
	token = flag.String("token", "test-token", "the custom security token")
	logLevel = flag.String("log", "info", "Log level (debug, info, warn, error, fatal, panic)")
	patchDir = flag.String("dir", "/tmp", "directory to store downloaded patches")
	patchWeb = flag.String("web", "https://s3.amazonaws.com/longsight-patches/", "website with patch files")
	localIP = flag.String("ip", "", "override automatic ip detection")
	startupWaitSeconds = flag.Int("waitTime", 280, "amount of time to wait for Tomcat to startup")

	flag.Parse()
	if len(*token) < 1 {
		fmt.Println("Please provide a valid security token")
		os.Exit(1)
	}

	// Set log level
	log.SetFormatter(&log.TextFormatter{})
	switch strings.ToLower(*logLevel) {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "fatal":
		log.SetLevel(log.FatalLevel)
	case "panic":
		log.SetLevel(log.PanicLevel)
	default:
		log.Warn("Unknown log level, defaulting to info")
		log.SetLevel(log.InfoLevel)
	}
}
