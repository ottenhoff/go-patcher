package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const patcherURL = "https://admin.longsight.com/longsight/json/patches"
const patcherUserAgent = "GoPatcher v1.0"
const processGrepPattern = "ps x|grep -v grep|grep java"

var token = flag.String("token", "", "the custom security token")
var patchDir = flag.String("dir", "/tmp", "directory to store downloaded patches")
var patchWeb = flag.String("web", "https://s3.amazonaws.com/longsight-patches/", "website with patch files")
var patcherUID = uint32(os.Getuid())

func init() {
	flag.Parse()
	if len(*token) < 1 {
		fmt.Println("Please provide a valid security token")
		os.Exit(1)
	}
}

func main() {
	ip, _ := externalIP()
	data := checkForPatchesFromPortal(ip)
	if len(data) < 1 {
		os.Exit(1)
	}

	tomcatDir := data["tomcat_dir"].(string)
	patchFiles := data["files"].(string)

	// Make sure the Tomcat directory exists on this host
	checkTomcatDirExists(tomcatDir)
	checkTomcatOwnership(tomcatDir)

	// Change working directory
	os.Chdir(tomcatDir)

	// Stop Tomcat
	out, _ := exec.Command("bin/catalina.sh stop").CombinedOutput()
	time.Sleep(10 * 1000 * time.Millisecond)
	hardKillProcess(tomcatDir)

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

	// Check if process still exists
	checkForProcess(tomcatDir)

	fmt.Println(out)
	fmt.Println(data)
}

func fetchTarball(tarball string) string {
	fullPath := tarball
	fileName := path.Base(tarball)

	// See if the file exists in local patch directory
	if !pathExists(fullPath) {
		fullPath = *patchDir + string(os.PathSeparator) + fileName
	}

	// See if we can pull file from S3
	if !pathExists(fullPath) {
		fileWriter, err := os.Create(fullPath)
		if err != nil {
			panic("Could not open file: " + fullPath)
		}
		defer fileWriter.Close()

		resp, err := http.Get(*patchWeb + "sakai-builder/" + fileName)
		fmt.Println(*patchWeb + "sakai-builder/" + fileName)
		fmt.Println(resp.StatusCode)
		if resp.StatusCode != http.StatusOK {
			resp, err = http.Get(*patchWeb + "patches/" + fileName)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			n, err := io.Copy(fileWriter, resp.Body)
			fmt.Println(n)
			if n > 0 && err != nil {
				panic("Could not copy from web to local file system")
			}
		}
	}

	if !pathExists(fullPath) {
		panic("Could not find the patch file: " + fileName)
	}

	return fullPath
}

func applyTarballPatch(tarball string) {
	filePath := fetchTarball(tarball)

	// Unroll the tarball one time to see what to clean out
	fileMap := unrollTarball(filePath)

	// Clean out old directories
	for fileMapPath, cnt := range fileMap {
		webappOrComponent := strings.HasPrefix(fileMapPath, "webapps") || strings.HasPrefix(fileMapPath, "components")
		if cnt > 3 && webappOrComponent {
			pathArray := strings.Split(fileMapPath, "/")
			pathToDelete := pathArray[0] + "/" + pathArray[1]
			err := os.RemoveAll(pathToDelete)
			if err != nil {
				panic("Could not remove path: " + pathToDelete)
			}
			//fmt.Println("delete: " + pathToDelete)
		}
	}

	// Unroll the tarball again after cleaning out old directories
	unrollTarball(filePath)
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
			//fmt.Println("Creating directory :", filename)
			err = os.MkdirAll(filename, os.FileMode(header.Mode)) // or use 0755 if you prefer

			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}

		case tar.TypeReg:
			// handle normal file

			// Strip files that start with dot slash
			startsWithDot := strings.HasPrefix(filename, "./")
			if startsWithDot {
				filename = strings.Replace(filename, "./", "", 1)
			}

			// See if there are any dirs we should wipe out
			if len(filename) > len("components/a") {
				splitPaths := strings.Split(filename, "/")
				if len(splitPaths) > 1 {
					//fmt.Println(splitPaths)
					firstTwoPaths := splitPaths[0] + "/" + splitPaths[1]

					_, ok := m[firstTwoPaths]
					if ok {
						m[firstTwoPaths]++
					} else {
						m[firstTwoPaths] = 1
					}
				}
			}

			//fmt.Println("Untarring :", filename)
			writer, err := os.Create(filename)

			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}

			io.Copy(writer, tarBallReader)

			err = os.Chmod(filename, os.FileMode(header.Mode))

			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}

			writer.Close()
		default:
			fmt.Printf("Unable to untar type : %c in file %s", header.Typeflag, filename)
		}
	}

	return m
}

func checkForProcess(tomcatDir string) bool {
	out, _ := exec.Command("bash", "-c", processGrepPattern+"|grep tomcat").Output()
	if len(out) > 0 {
		return true
	}

	return false
}

func hardKillProcess(tomcatDir string) {
	alive := checkForProcess(tomcatDir)
	//fmt.Printf("alive: %v \n", alive)

	if alive {
		out, _ := exec.Command("bash", "-c", processGrepPattern+"|grep "+tomcatDir).Output()
		p := strings.SplitN(string(out), " ", 3)
		for _, ps := range p {
			k, err := strconv.Atoi(ps)
			if err == nil && k > 100 {
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
		}
	} else {
		fmt.Printf("Bad HTTP fetch: %v \n", resp.Status)
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
	if tomcatUID != patcherUID {
		panic("Patcher UID is different from Tomcat UID")
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
