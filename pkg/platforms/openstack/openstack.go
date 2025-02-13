package openstack

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/jaypipes/ghw"
	"github.com/jaypipes/ghw/pkg/net"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dputils "github.com/k8snetworkplumbingwg/sriov-network-device-plugin/pkg/utils"

	sriovnetworkv1 "github.com/k8snetworkplumbingwg/sriov-network-operator/api/v1"
	"github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/consts"
	"github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/host"
)

const (
	ospHostMetaDataDir     = "/host/var/config/openstack/2018-08-27"
	ospMetaDataDir         = "/var/config/openstack/2018-08-27"
	ospMetaDataBaseURL     = "http://169.254.169.254/openstack/2018-08-27"
	ospNetworkDataJSON     = "network_data.json"
	ospMetaDataJSON        = "meta_data.json"
	ospHostNetworkDataFile = ospHostMetaDataDir + "/" + ospNetworkDataJSON
	ospHostMetaDataFile    = ospHostMetaDataDir + "/" + ospMetaDataJSON
	ospNetworkDataURL      = ospMetaDataBaseURL + "/" + ospNetworkDataJSON
	ospMetaDataURL         = ospMetaDataBaseURL + "/" + ospMetaDataJSON
)

var (
	ospNetworkDataFile = ospMetaDataDir + "/" + ospNetworkDataJSON
	ospMetaDataFile    = ospMetaDataDir + "/" + ospMetaDataJSON
)

//go:generate ../../../bin/mockgen -destination mock/mock_openstack.go -source openstack.go
type OpenstackInterface interface {
	CreateOpenstackDevicesInfo() error
	CreateOpenstackDevicesInfoFromNodeStatus(*sriovnetworkv1.SriovNetworkNodeState)
	DiscoverSriovDevicesVirtual() ([]sriovnetworkv1.InterfaceExt, error)
}

type openstackContext struct {
	hostManager          host.HostManagerInterface
	openStackDevicesInfo OSPDevicesInfo
}

// OSPMetaDataDevice -- Device structure within meta_data.json
type OSPMetaDataDevice struct {
	Vlan      int      `json:"vlan,omitempty"`
	VfTrusted bool     `json:"vf_trusted,omitempty"`
	Type      string   `json:"type,omitempty"`
	Mac       string   `json:"mac,omitempty"`
	Bus       string   `json:"bus,omitempty"`
	Address   string   `json:"address,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

// OSPMetaData -- Openstack meta_data.json format
type OSPMetaData struct {
	UUID             string              `json:"uuid,omitempty"`
	AdminPass        string              `json:"admin_pass,omitempty"`
	Name             string              `json:"name,omitempty"`
	LaunchIndex      int                 `json:"launch_index,omitempty"`
	AvailabilityZone string              `json:"availability_zone,omitempty"`
	ProjectID        string              `json:"project_id,omitempty"`
	Devices          []OSPMetaDataDevice `json:"devices,omitempty"`
}

// OSPNetworkLink OSP Link metadata
type OSPNetworkLink struct {
	ID          string `json:"id"`
	VifID       string `json:"vif_id,omitempty"`
	Type        string `json:"type"`
	Mtu         int    `json:"mtu,omitempty"`
	EthernetMac string `json:"ethernet_mac_address"`
}

// OSPNetwork OSP Network metadata
type OSPNetwork struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Link      string `json:"link"`
	NetworkID string `json:"network_id"`
}

// OSPNetworkData OSP Network metadata
type OSPNetworkData struct {
	Links    []OSPNetworkLink `json:"links,omitempty"`
	Networks []OSPNetwork     `json:"networks,omitempty"`
	// Omit Services
}

type OSPDevicesInfo map[string]*OSPDeviceInfo

type OSPDeviceInfo struct {
	MacAddress string
	NetworkID  string
}

func New(hostManager host.HostManagerInterface) OpenstackInterface {
	return &openstackContext{
		hostManager: hostManager,
	}
}

// GetOpenstackData gets the metadata and network_data
func getOpenstackData(useHostPath bool) (metaData *OSPMetaData, networkData *OSPNetworkData, err error) {
	metaData, networkData, err = getOpenstackDataFromConfigDrive(useHostPath)
	if err != nil {
		metaData, networkData, err = getOpenstackDataFromMetadataService()
		if err != nil {
			return metaData, networkData, fmt.Errorf("GetOpenStackData(): error getting OpenStack data: %w", err)
		}
	}

	// We can't rely on the PCI address from the metadata so we will lookup the real PCI address
	// for the NIC that matches the MAC address.
	//
	// Libvirt/QEMU cannot guarantee that the address specified in the XML will match the address seen by the guest.
	// This is a well known limitation: https://libvirt.org/pci-addresses.html
	// When using the q35 machine type, it highlights this issue due to the change from using PCI to PCI-E bus for virtual devices.
	//
	// With that said, the PCI value in Nova Metadata is a best effort hint due to the limitations mentioned above. Therefore
	// we will lookup the real PCI address for the NIC that matches the MAC address.
	netInfo, err := ghw.Network()
	if err != nil {
		return metaData, networkData, fmt.Errorf("GetOpenStackData(): error getting network info: %w", err)
	}
	for i, device := range metaData.Devices {
		realPCIAddr, err := getPCIAddressFromMACAddress(device.Mac, netInfo.NICs)
		if err != nil {
			// If we can't find the PCI address, we will just print a warning, return the data as is with no error.
			// In the future, we'll want to drain the node if sno-initial-node-state.json doesn't exist when daemon is restarted and when we have SR-IOV
			// allocated devices already.
			log.Log.Error(err, "Warning GetOpenstackData(): error getting PCI address for device",
				"device-mac", device.Mac)
			return metaData, networkData, nil
		}
		if realPCIAddr != device.Address {
			log.Log.V(2).Info("GetOpenstackData(): PCI address for device does not match Nova metadata value, it'll be overwritten",
				"device-mac", device.Mac,
				"current-address", device.Address,
				"overwrite-address", realPCIAddr)
			metaData.Devices[i].Address = realPCIAddr
		}
	}

	return metaData, networkData, err
}

// getOpenstackDataFromConfigDrive reads the meta_data and network_data files
func getOpenstackDataFromConfigDrive(useHostPath bool) (metaData *OSPMetaData, networkData *OSPNetworkData, err error) {
	metaData = &OSPMetaData{}
	networkData = &OSPNetworkData{}
	log.Log.Info("reading OpenStack meta_data from config-drive")
	var metadataf *os.File
	ospMetaDataFilePath := ospMetaDataFile
	if useHostPath {
		ospMetaDataFilePath = ospHostMetaDataFile
	}
	metadataf, err = os.Open(ospMetaDataFilePath)
	if err != nil {
		return metaData, networkData, fmt.Errorf("error opening file %s: %w", ospHostMetaDataFile, err)
	}
	defer func() {
		if e := metadataf.Close(); err == nil && e != nil {
			err = fmt.Errorf("error closing file %s: %w", ospHostMetaDataFile, e)
		}
	}()
	if err = json.NewDecoder(metadataf).Decode(&metaData); err != nil {
		return metaData, networkData, fmt.Errorf("error unmarshalling metadata from file %s: %w", ospHostMetaDataFile, err)
	}

	log.Log.Info("reading OpenStack network_data from config-drive")
	var networkDataf *os.File
	ospNetworkDataFilePath := ospNetworkDataFile
	if useHostPath {
		ospNetworkDataFilePath = ospHostNetworkDataFile
	}
	networkDataf, err = os.Open(ospNetworkDataFilePath)
	if err != nil {
		return metaData, networkData, fmt.Errorf("error opening file %s: %w", ospHostNetworkDataFile, err)
	}
	defer func() {
		if e := networkDataf.Close(); err == nil && e != nil {
			err = fmt.Errorf("error closing file %s: %w", ospHostNetworkDataFile, e)
		}
	}()
	if err = json.NewDecoder(networkDataf).Decode(&networkData); err != nil {
		return metaData, networkData, fmt.Errorf("error unmarshalling metadata from file %s: %w", ospHostNetworkDataFile, err)
	}
	return metaData, networkData, err
}

func getBodyFromURL(url string) ([]byte, error) {
	log.Log.V(2).Info("Getting body from", "url", url)
	resp, err := retryablehttp.Get(url)
	if err != nil {
		return nil, err
	}
	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return rawBytes, nil
}

// getOpenstackDataFromMetadataService fetchs the metadata and network_data from the metadata service
func getOpenstackDataFromMetadataService() (metaData *OSPMetaData, networkData *OSPNetworkData, err error) {
	metaData = &OSPMetaData{}
	networkData = &OSPNetworkData{}
	log.Log.Info("getting OpenStack meta_data from metadata server")
	metaDataRawBytes, err := getBodyFromURL(ospMetaDataURL)
	if err != nil {
		return metaData, networkData, fmt.Errorf("error getting OpenStack meta_data from %s: %v", ospMetaDataURL, err)
	}
	err = json.Unmarshal(metaDataRawBytes, metaData)
	if err != nil {
		return metaData, networkData, fmt.Errorf("error unmarshalling raw bytes %v from %s", err, ospMetaDataURL)
	}

	log.Log.Info("getting OpenStack network_data from metadata server")
	networkDataRawBytes, err := getBodyFromURL(ospNetworkDataURL)
	if err != nil {
		return metaData, networkData, fmt.Errorf("error getting OpenStack network_data from %s: %v", ospNetworkDataURL, err)
	}
	err = json.Unmarshal(networkDataRawBytes, networkData)
	if err != nil {
		return metaData, networkData, fmt.Errorf("error unmarshalling raw bytes %v from %s", err, ospNetworkDataURL)
	}
	return metaData, networkData, nil
}

// getPCIAddressFromMACAddress returns the PCI address of a device given its MAC address
func getPCIAddressFromMACAddress(macAddress string, nics []*net.NIC) (string, error) {
	var pciAddress string
	for _, nic := range nics {
		if strings.EqualFold(nic.MacAddress, macAddress) {
			if pciAddress == "" {
				pciAddress = *nic.PCIAddress
			} else {
				return "", fmt.Errorf("more than one device found with MAC address %s is unsupported", macAddress)
			}
		}
	}

	if pciAddress != "" {
		return pciAddress, nil
	}

	return "", fmt.Errorf("no device found with MAC address %s", macAddress)
}

// CreateOpenstackDevicesInfo create the openstack device info map
func (o *openstackContext) CreateOpenstackDevicesInfo() error {
	log.Log.Info("CreateOpenstackDevicesInfo()")
	devicesInfo := make(OSPDevicesInfo)

	metaData, networkData, err := getOpenstackData(true)
	if err != nil {
		log.Log.Error(err, "failed to read OpenStack data")
		return err
	}

	if metaData == nil || networkData == nil {
		o.openStackDevicesInfo = make(OSPDevicesInfo)
		return nil
	}

	// use this for hw pass throw interfaces
	for _, device := range metaData.Devices {
		for _, link := range networkData.Links {
			if device.Mac == link.EthernetMac {
				for _, network := range networkData.Networks {
					if network.Link == link.ID {
						networkID := sriovnetworkv1.OpenstackNetworkID.String() + ":" + network.NetworkID
						devicesInfo[device.Address] = &OSPDeviceInfo{MacAddress: device.Mac, NetworkID: networkID}
					}
				}
			}
		}
	}

	// for vhostuser interface type we check the interfaces on the node
	pci, err := ghw.PCI()
	if err != nil {
		return fmt.Errorf("CreateOpenstackDevicesInfo(): error getting PCI info: %v", err)
	}

	devices := pci.ListDevices()
	if len(devices) == 0 {
		return fmt.Errorf("CreateOpenstackDevicesInfo(): could not retrieve PCI devices")
	}

	for _, device := range devices {
		if _, exist := devicesInfo[device.Address]; exist {
			//we already discover the device via openstack metadata
			continue
		}

		devClass, err := strconv.ParseInt(device.Class.ID, 16, 64)
		if err != nil {
			log.Log.Error(err, "CreateOpenstackDevicesInfo(): unable to parse device class for device, skipping",
				"device", device)
			continue
		}
		if devClass != consts.NetClass {
			// Not network device
			continue
		}

		macAddress := ""
		if name := o.hostManager.TryToGetVirtualInterfaceName(device.Address); name != "" {
			if mac := o.hostManager.GetNetDevMac(name); mac != "" {
				macAddress = mac
			}
		}
		if macAddress == "" {
			// we didn't manage to find a mac address for the nic skipping
			continue
		}

		for _, link := range networkData.Links {
			if macAddress == link.EthernetMac {
				for _, network := range networkData.Networks {
					if network.Link == link.ID {
						networkID := sriovnetworkv1.OpenstackNetworkID.String() + ":" + network.NetworkID
						devicesInfo[device.Address] = &OSPDeviceInfo{MacAddress: macAddress, NetworkID: networkID}
					}
				}
			}
		}
	}

	o.openStackDevicesInfo = devicesInfo
	return nil
}

// DiscoverSriovDevicesVirtual discovers VFs on a virtual platform
func (o *openstackContext) DiscoverSriovDevicesVirtual() ([]sriovnetworkv1.InterfaceExt, error) {
	log.Log.V(2).Info("DiscoverSriovDevicesVirtual()")
	pfList := []sriovnetworkv1.InterfaceExt{}

	pci, err := ghw.PCI()
	if err != nil {
		return nil, fmt.Errorf("DiscoverSriovDevicesVirtual(): error getting PCI info: %v", err)
	}

	devices := pci.ListDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("DiscoverSriovDevicesVirtual(): could not retrieve PCI devices")
	}

	for _, device := range devices {
		devClass, err := strconv.ParseInt(device.Class.ID, 16, 64)
		if err != nil {
			log.Log.Error(err, "DiscoverSriovDevicesVirtual(): unable to parse device class for device, skipping",
				"device", device)
			continue
		}
		if devClass != consts.NetClass {
			// Not network device
			continue
		}

		deviceInfo, exist := o.openStackDevicesInfo[device.Address]
		if !exist {
			log.Log.Error(nil, "DiscoverSriovDevicesVirtual(): unable to find device in devicesInfo list, skipping",
				"device", device.Address)
			continue
		}
		netFilter := deviceInfo.NetworkID
		metaMac := deviceInfo.MacAddress

		driver, err := dputils.GetDriverName(device.Address)
		if err != nil {
			log.Log.Error(err, "DiscoverSriovDevicesVirtual(): unable to parse device driver for device, skipping",
				"device", device)
			continue
		}
		iface := sriovnetworkv1.InterfaceExt{
			PciAddress: device.Address,
			Driver:     driver,
			Vendor:     device.Vendor.ID,
			DeviceID:   device.Product.ID,
			NetFilter:  netFilter,
		}
		if mtu := o.hostManager.GetNetdevMTU(device.Address); mtu > 0 {
			iface.Mtu = mtu
		}
		if name := o.hostManager.TryToGetVirtualInterfaceName(device.Address); name != "" {
			iface.Name = name
			if iface.Mac = o.hostManager.GetNetDevMac(name); iface.Mac == "" {
				iface.Mac = metaMac
			}
			iface.LinkSpeed = o.hostManager.GetNetDevLinkSpeed(name)
		}
		iface.LinkType = o.hostManager.GetLinkType(iface)

		iface.TotalVfs = 1
		iface.NumVfs = 1

		vf := sriovnetworkv1.VirtualFunction{
			PciAddress: device.Address,
			Driver:     driver,
			VfID:       0,
			Vendor:     iface.Vendor,
			DeviceID:   iface.DeviceID,
			Mtu:        iface.Mtu,
			Mac:        iface.Mac,
		}
		iface.VFs = append(iface.VFs, vf)

		pfList = append(pfList, iface)
	}
	return pfList, nil
}

func (o *openstackContext) CreateOpenstackDevicesInfoFromNodeStatus(networkState *sriovnetworkv1.SriovNetworkNodeState) {
	devicesInfo := make(OSPDevicesInfo)
	for _, iface := range networkState.Status.Interfaces {
		devicesInfo[iface.PciAddress] = &OSPDeviceInfo{MacAddress: iface.Mac, NetworkID: iface.NetFilter}
	}

	o.openStackDevicesInfo = devicesInfo
}
