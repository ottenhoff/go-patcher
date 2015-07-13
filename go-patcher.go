package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const patcherURL = "https://admin.longsight.com/longsight/json/patches"
const patcherUserAgent = "GoPatcher v1.0"
const processGrepPattern = "/bin/ps x|grep -v grep|grep java"

var token = flag.String("token", "", "the custom security token")
var patchDir = flag.String("dir", "/tmp", "directory to store downloaded patches")
var patcherUID = uint32(os.Getuid())

func init() {
	flag.Parse()
	if len(*token) < 1 {
		panic("Please provide a valid security token")
	}
}

func main() {
	ip, _ := externalIP()
	data := checkForPatchesFromPortal(ip)

	tomcatDir := data["tomcat_dir"].(string)
	patchFiles := data["files"].(string)

	// Make sure the Tomcat directory exists on this host
	checkTomcatDirExists(tomcatDir)
	checkTomcatOwnership(tomcatDir)

	// Stop Tomcat
	out, _ := exec.Command("cd " + tomcatDir + " && bin/catalina.sh stop").CombinedOutput()
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

func applyTarballPatch(tarball string) {
	path := tarball
	if !pathExists(path) {
		path = *patchDir + string(os.PathSeparator) + tarball
	}
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

	if resp.Status == "200 OK" {
		body, _ := ioutil.ReadAll(resp.Body)

		// We have a real patch
		if len(body) > 5 {
			json.Unmarshal(body, &data)
		}
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
