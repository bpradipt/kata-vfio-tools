package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

var (
	// version is the version string of the hook. Set at build time.
	version = "0.1"
	log     = logrus.New()
	//List taken from
	// https://docs.openshift.com/container-platform/4.2/networking/multiple_networks/configuring-sr-iov.html#supported-devices_configuring-sr-iov
	// 0x10ec:0x522a added for testing on my laptop (it's an SD
	// card reader which is interesting solely because I'm not
	// generally using it on the host) -dgibson
	pciSupportedVendorDeviceList = []string{"0x8086:0x1521", "0x8086:0x1520", "0x8086:0x158b", "0x15b3:0x1015", "0x15b3:0x1017", "0x10ec:0x522a"}
)

const (
	pciDeviceFile  = "/sys/bus/pci/devices"
	vfioDeviceFile = "/sys/bus/pci/drivers/vfio-pci"
)

func main() {

	log.Out = os.Stdout

	dname, err := ioutil.TempDir("", "vfiohooklog")
	fname := filepath.Join(dname, "vfiohook.log")
	file, err := os.OpenFile(fname, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.Infof("Log file: %s", fname)
		log.Out = file
	} else {
		log.Info("Failed to log to file, using default stderr")
	}
	//logrus.SetLevel(logrus.DebugLevel)
	log.Infof("Started VFIO OCI hook version %s", version)

	start := flag.Bool("s", true, "Start the VFIO hook")
	printVersion := flag.Bool("version", false, "Print the hook's version")
	flag.Parse()

	if *printVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *start {
		log.Info("Starting VFIO hook")
		if err := startVfioOciHook(); err != nil {
			//Hook should not fail
			//log.Fatal(err)
			log.Info(err)
			return
		}
	}
}

func startVfioOciHook() error {
	//Hook receives container State in Stdin
	//https://github.com/opencontainers/runtime-spec/blob/master/config.md#posix-platform-hooks
	//https://github.com/opencontainers/runtime-spec/blob/master/runtime.md#state

	var s spec.State
	reader := bufio.NewReader(os.Stdin)
	decoder := json.NewDecoder(reader)
	err := decoder.Decode(&s)
	if err != nil {
		return err
	}

	//log spec State to file
	log.Infof("spec.State is %v", s)

	bundlePath := s.Bundle
	containerPid := s.Pid

	log.Infof("Rootfs for container (%d) is at: %s", containerPid, bundlePath)

	//For Kata the config.json is in a different path
	configJsonPath := filepath.Join("/run/libcontainer", s.ID, "config.json")

	log.Infof("Config.json location: %s", configJsonPath)
	//Read the JSON
	jsonData, err := ioutil.ReadFile(configJsonPath)
	if err != nil {
		log.Errorf("unable to read config.json %s", err)
		return err
	}

	log.Debugf("Config.json contents: %s", jsonData)

	err = bindVFIO()
	if err != nil {
		log.Infof("Error in binding device to vfio driver", err)
		return err
	}

	return nil
}

//Bind each supported vendor:device to vfio-pci
func bindVFIO() error {

	log.Infof("bindVFIO: Start")

	//Get PCI device list
	//For each PCI device in the list get vendor:device and create a map
	//key:"vendor:device", value:"device"
	devMap := createDeviceMap()

	//For each matching key:"vendor:device", rebind driver
	if len(devMap) != 0 {
		doRebind(devMap)
	}

	return nil
}

//Create a Map of vendor:device and corresponding pci:bdf
func createDeviceMap() map[string]string {

	log.Infof("creating DeviceMap")
	deviceMap := make(map[string]string)

	bdfList, err := ioutil.ReadDir(pciDeviceFile)
	if err != nil {
		log.Errorf("Unable to get device list %s", err)
		return nil
	}

	for _, bdf := range bdfList {
		vendorPath := filepath.Join(pciDeviceFile, bdf.Name(), "vendor")
		devicePath := filepath.Join(pciDeviceFile, bdf.Name(), "device")
		vendor, err := ioutil.ReadFile(vendorPath)
		if err != nil {
			log.Errorf("Fetching vendor id for device(%s) returned error: %s", bdf, err)
			continue
		}
		device, err := ioutil.ReadFile(devicePath)
		if err != nil {
			log.Errorf("Fetching device id for device(%s) returned error: %s", bdf, err)
			continue
		}
		key := fmt.Sprintf("%s:%s", strings.TrimSuffix(string(vendor), "\n"), strings.TrimSuffix(string(device), "\n"))
		deviceMap[key] = bdf.Name()
	}

	log.Debugf("DeviceMap %v", deviceMap)

	return deviceMap

}

//Rebind the devices to vfio-pci driver
func doRebind(deviceMap map[string]string) error {

	log.Infof("Rebinding driver for the devices")
	//Find if supported vendor:device is there in the device map
	for key, element := range deviceMap {
		log.Debugf("DeviceMap entries: vd: %s => bdf: %s", key, element)
	}

	for _, vd := range pciSupportedVendorDeviceList {
		if bdf, found := deviceMap[vd]; found {
			err := rebindOne(bdf, vd)
			if err != nil {
				log.Errorf("Error rebinding %s: %s", bdf, err)
				continue
			}
		}
	}
	return nil
}

func rebindOne(bdf string, vd string) error {
	log.Infof("Found device ", bdf)

	driverPath := filepath.Join(pciDeviceFile, bdf, "driver")
	if _, err := os.Stat(driverPath); err == nil {
		driver, err := os.Readlink(driverPath)
		if err != nil {
			return fmt.Errorf("Could not read driver: %s", err)
		}
		if string(driver) == "vfio-pci" {
			log.Infof("%s is already bound to vfio", bdf)
			return nil
		} else {
			log.Infof("Unbinding %s from current driver", bdf)
			unbindPath := filepath.Join(pciDeviceFile, bdf, "driver/unbind")
			err = ioutil.WriteFile(unbindPath, []byte(bdf), 0200)
			if err != nil {
				return fmt.Errorf("Could not unbind driver: %s", err)
			}
		}
	}

	log.Infof("Binding device (%s) to vfio", bdf)
	newidPath := filepath.Join(vfioDeviceFile, "new_id")
	newid := strings.Replace(vd, ":", " ", 1)
	err := ioutil.WriteFile(newidPath, []byte(newid), 0200)
	if err != nil {
		return fmt.Errorf("Could not bind vfio driver: %s", err)
	}
	log.Infof("Successfully rebound %s to vfio", bdf)
	return nil
}
