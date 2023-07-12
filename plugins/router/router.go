package main

import (
	"encoding/json"
	"fmt"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/spidernet-io/cni-plugins/pkg/config"
	"github.com/spidernet-io/cni-plugins/pkg/constant"
	"github.com/spidernet-io/cni-plugins/pkg/logging"
	"github.com/spidernet-io/cni-plugins/pkg/networking"
	ty "github.com/spidernet-io/cni-plugins/pkg/types"
	"github.com/spidernet-io/cni-plugins/pkg/utils"
	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"k8s.io/utils/pointer"
	"net"
	"os"
	"path/filepath"
	"time"
)

type PluginConf struct {
	types.NetConf
	// should include: overlay Subnet , clusterIP subnet
	// should include: overlay Subnet , clusterip subnet
	OverlayHijackSubnet     []string         `json:"overlay_hijack_subnet,omitempty"`
	ServiceHijackSubnet     []string         `json:"service_hijack_subnet,omitempty"`
	AdditionalHijackSubnet  []string         `json:"additional_hijack_subnet,omitempty"`
	MigrateRoute            *ty.MigrateRoute `json:"migrate_route,omitempty"`
	DefaultOverlayInterface string           `json:"overlay_interface,omitempty"`
	HostRuleTable           *int             `json:"host_rule_table,omitempty"`
	// RpFilter
	RPFilter   *ty.RPFilter   `json:"rp_filter,omitempty"`
	Skipped    bool           `json:"skip_call,omitempty"`
	Sriov      bool           `json:"sriov,omitempty"`
	OnlyOpMac  bool           `json:"only_op_mac,omitempty"`
	LogOptions *ty.LogOptions `json:"log_options,omitempty"`
	IPConflict *ty.IPConflict `json:"ip_conflict,omitempty"`
	MacPrefix  string         `json:"mac_prefix,omitempty"`
}

var binName = filepath.Base(os.Args[0])

var overlayRouteTable = 100

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString(binName))
}

func cmdAdd(args *skel.CmdArgs) error {
	startTime := time.Now()

	var logger *zap.Logger

	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	if err := logging.SetLogOptions(conf.LogOptions); err != nil {
		return fmt.Errorf("faild to init logger: %v ", err)
	}

	logger = logging.LoggerFile.Named(binName)

	k8sArgs := ty.K8sArgs{}
	if err = types.LoadArgs(args.Args, &k8sArgs); nil != err {
		logger.Error(err.Error())
		return fmt.Errorf("failed to get pod information, error=%+v \n", err)
	}

	// register some args into logger
	logger = logger.With(zap.String("Action", "Add"),
		zap.String("ContainerID", args.ContainerID),
		zap.String("PodUID", string(k8sArgs.K8S_POD_UID)),
		zap.String("PodName", string(k8sArgs.K8S_POD_NAME)),
		zap.String("PodNamespace", string(k8sArgs.K8S_POD_NAMESPACE)),
		zap.String("IfName", args.IfName))

	logger.Info("stdin", zap.String("stdin", string(args.StdinData)))
	logger.Debug("Succeed to parse cni config", zap.Any("Config", *conf))

	// skip plugin
	if conf.Skipped {
		logger.Info("Ignore this plugin call, Return directly ")
		return types.PrintResult(conf.PrevResult, conf.CNIVersion)
	}
	if conf.PrevResult == nil {
		logger.Error("failed to find PrevResult, must be called as chained plugin")
		return fmt.Errorf("failed to find PrevResult, must be called as chained plugin")
	}

	// ------------------- parse prevResult
	prevResult, err := current.GetResult(conf.PrevResult)
	if err != nil {
		logger.Error(err.Error())
		return fmt.Errorf("failed to convert prevResult: %v", err)
	}

	if len(prevResult.Interfaces) == 0 {
		err = fmt.Errorf("failed to find interface from prevResult")
		logger.Error(err.Error())
		return err
	}

	preInterfaceName := prevResult.Interfaces[0].Name
	if len(preInterfaceName) == 0 {
		err = fmt.Errorf("failed to find interface name from prevResult")
		logger.Error(err.Error())
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		logger.Error(err.Error())
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	// we do check if ip is conflict firstly
	if conf.IPConflict != nil && conf.IPConflict.Enabled {
		err = networking.DoIPConflictChecking(logger, netns, args.IfName, prevResult.IPs, conf.IPConflict)
		if err != nil {
			logger.Error(err.Error())
			return err
		}
	}

	if len(conf.MacPrefix) != 0 {
		newMac, err := utils.OverwriteMacAddress(logger, netns, conf.MacPrefix, args.IfName)
		if err != nil {
			return fmt.Errorf("failed to update mac address, maybe mac_prefix is invalid: %v", conf.MacPrefix)
		}
		logger.Info("Update mac address successfully", zap.String("interface", constant.DefaultInterfaceName), zap.String("new mac", newMac))
		if conf.OnlyOpMac {
			logger.Debug("only update mac address, exiting now...")
			return types.PrintResult(conf.PrevResult, conf.CNIVersion)
		}
	}

	enableIpv4 := false
	enableIpv6 := false
	for _, v := range prevResult.IPs {
		if v.Address.IP.To4() != nil {
			enableIpv4 = true
		} else {
			enableIpv6 = true
		}
	}

	// Pass the prevResult through this plugin to the next one
	// result := prevResult
	chainedInterfaceIps, err := utils.GetChainedInterfaceIps(netns, preInterfaceName, enableIpv4, enableIpv6)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	ruleTable := utils.GetRuleNumber(preInterfaceName)
	if ruleTable < 0 {
		logger.Error("failed to get the number of rule table for interface", zap.String("interface", preInterfaceName))
		return fmt.Errorf("failed to get the number of rule table for interface %s", preInterfaceName)
	}

	// setup neighborhood to fix pod and host communication issue
	if err = utils.AddStaticNeighTable(logger, netns, conf.Sriov, enableIpv4, enableIpv6, conf.DefaultOverlayInterface, chainedInterfaceIps); err != nil {
		logger.Error(err.Error())
		return err
	}

	if enableIpv6 {
		if err = utils.EnableIpv6Sysctl(logger, netns); err != nil {
			logger.Error(err.Error())
			return err
		}
	}

	// ----------------- Add route table in host ns
	if err = addChainedIPRoute(logger, netns, conf.Sriov, enableIpv4, enableIpv6, *conf.HostRuleTable, conf.DefaultOverlayInterface, chainedInterfaceIps); err != nil {
		logger.Error(err.Error())
		return err
	}

	// -----------------  Add route table in pod ns
	// add route in pod: hostIP via DefaultOverlayInterface
	if err = addHostIPRoute(logger, netns, ruleTable, conf.DefaultOverlayInterface, conf.Sriov, enableIpv4, enableIpv6); err != nil {
		logger.Error("failed to add host ip route in container", zap.Error(err))
		return fmt.Errorf("failed to add route: %v", err)
	}

	// hijack overlay response packet to overlay interface
	// we move default route into table <ruleTable>.
	defaultInterfaceIPs, err := utils.GetChainedInterfaceIps(netns, utils.GetDefaultRouteInterface(preInterfaceName), enableIpv4, enableIpv6)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	// add route in pod: custom subnet via DefaultOverlayInterface:  overlay subnet / clusterip subnet ...custom route
	if err = utils.HijackCustomSubnet(logger, netns, conf.ServiceHijackSubnet, conf.OverlayHijackSubnet, conf.AdditionalHijackSubnet, defaultInterfaceIPs, ruleTable, enableIpv4, enableIpv6); err != nil {
		logger.Error(err.Error())
		return err
	}

	if err = utils.MigrateRoute(logger, netns, utils.GetDefaultRouteInterface(preInterfaceName), preInterfaceName, defaultInterfaceIPs, *conf.MigrateRoute, ruleTable, enableIpv4, enableIpv6); err != nil {
		logger.Error(err.Error())
		return err
	}

	// setup sysctl rp_filter
	if err = utils.SysctlRPFilter(logger, netns, conf.RPFilter); err != nil {
		logger.Error(err.Error())
		return err
	}

	logger.Info("Succeeded to set for chained interface for overlay interface",
		zap.String("interface", preInterfaceName), zap.Int64("Time Cost", time.Since(startTime).Microseconds()))

	return types.PrintResult(conf.PrevResult, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	// delete rule table on host
	var logger *zap.Logger

	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	if err := logging.SetLogOptions(conf.LogOptions); err != nil {
		return fmt.Errorf("faild to init logger: %v ", err)
	}

	logger = logging.LoggerFile.Named(binName)

	k8sArgs := ty.K8sArgs{}
	if err = types.LoadArgs(args.Args, &k8sArgs); nil != err {
		logger.Error(err.Error())
		return fmt.Errorf("failed to get pod information, error=%+v \n", err)
	}

	// register some args into logger
	logger = logger.With(zap.String("Action", "Del"),
		zap.String("ContainerID", args.ContainerID),
		zap.String("PodUID", string(k8sArgs.K8S_POD_UID)),
		zap.String("PodName", string(k8sArgs.K8S_POD_NAME)),
		zap.String("PodNamespace", string(k8sArgs.K8S_POD_NAMESPACE)),
		zap.String("IfName", args.IfName))

	logger.Debug("Start call CmdDel for Router-plugin", zap.Any("Config", *conf))
	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		if _, ok := err.(ns.NSPathNotExistErr); ok {
			return nil
		}
		logger.Error(err.Error())
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	chainedInterfaceIps, err := utils.GetChainedInterfaceIps(netns, args.IfName, true, true)
	if err != nil {
		logger.Warn("Pod No IPs, Skip call CmdDel", zap.Error(err))
	}
	logger.Debug("Get ChainedInterface IPs", zap.String("interface", args.IfName), zap.Strings("IPs", chainedInterfaceIps))
	if err = utils.RuleDel(logger, *conf.HostRuleTable, chainedInterfaceIps); err != nil {
		logger.Error(err.Error())
		return err
	}
	logger.Debug("Succeed to call CmdDel for Router-Plugin")
	return err
}

func cmdCheck(args *skel.CmdArgs) error {
	// TODO
	return fmt.Errorf("not implement it")
}

// parseConfig parses the supplied configuration (and prevResult) from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	var err error
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("[router] failed to parse network configuration: %v", err)
	}

	// Parse previous result. This will parse, validate, and place the
	// previous result object into conf.PrevResult. If you need to modify
	// or inspect the PrevResult you will need to convert it to a concrete
	// versioned Result struct.
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return nil, fmt.Errorf("[router] could not parse prevResult: %v", err)
	}
	// End previous result parsing
	if err = config.ValidateOverwriteMacAddress(conf.MacPrefix); err != nil {
		return nil, err
	}

	conf.LogOptions = logging.InitLogOptions(conf.LogOptions)
	if conf.LogOptions.LogFilePath == "" {
		conf.LogOptions.LogFilePath = constant.RouterLogDefaultFilePath
	}

	if conf.OnlyOpMac {
		return &conf, nil
	}

	conf.ServiceHijackSubnet, conf.OverlayHijackSubnet, err = config.ValidateRoutes(conf.ServiceHijackSubnet, conf.OverlayHijackSubnet)
	if err != nil {
		return nil, err
	}

	if conf.IPConflict != nil {
		conf.IPConflict = config.ValidateIPConflict(conf.IPConflict)
		_, err = time.ParseDuration(conf.IPConflict.Interval)
		if err != nil {
			return nil, fmt.Errorf("invalid interval %s: %v, input like: 1s,1m", conf.IPConflict.Interval, err)
		}
	}

	conf.MigrateRoute = config.ValidateMigrateRouteConfig(conf.MigrateRoute)

	if conf.DefaultOverlayInterface == "" {
		conf.DefaultOverlayInterface = "eth0"
	}

	if conf.HostRuleTable == nil {
		conf.HostRuleTable = pointer.Int(500)
	}

	// value must be 0/1/2
	// If not, giving default value: RPFilter_Loose(2) to it
	conf.RPFilter = config.ValidateRPFilterConfig(conf.RPFilter)

	return &conf, nil
}

// addHostIPRoute add all routes to the node in pod netns, the nexthop is the ip of the host
// only add to main!
func addHostIPRoute(logger *zap.Logger, netns ns.NetNS, ruleTable int, defaultInterface string, iSriov, enableIpv4 bool, enableIpv6 bool) error {
	if iSriov {
		logger.Info("Main-cni is sriov, don't need to set chained route")
		return nil
	}
	var err error
	hostIPs, err := utils.GetHostIps(logger, enableIpv4, enableIpv6)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	logger.Debug("addHostIPRoute add hostIP dev eth0",
		zap.Strings("Host IPs", hostIPs),
		zap.Int("RuleTable", ruleTable),
		zap.Bool("enableIpv4", enableIpv4),
		zap.Bool("enableIpv6", enableIpv6))
	err = netns.Do(func(_ ns.NetNS) error {
		if ruleTable == overlayRouteTable {
			logger.Debug("addHostIPRoute add hostIP route dev eth0 to table main")
			if _, _, err = utils.RouteAdd(logger, unix.RT_TABLE_MAIN, defaultInterface, hostIPs, enableIpv4, enableIpv6); err != nil {
				logger.Error(err.Error())
				return err
			}
		}
		// add route in pod: hostIP via DefaultOverlayInterface
		if _, _, err = utils.RouteAdd(logger, ruleTable, defaultInterface, hostIPs, enableIpv4, enableIpv6); err != nil {
			return err
		}
		return nil
	})
	return err
}

// addChainedIPRoute to solve macvlan master/slave interface can't communications directly, we add a route fix it.
// something like: ip r add <macvlan_ip> dev <overlay_veth_device> on host
func addChainedIPRoute(logger *zap.Logger, netNS ns.NetNS, iSriov, enableIpv4, enableIpv6 bool, hostRuleTable int, defaultOverlayInterface string, chainedIPs []string) error {
	if iSriov {
		logger.Debug("main-cni is sriov, don't need set chained route")
		return nil
	}
	// 1. get defaultOverlayInterface IP
	logger.Debug("Add underlay interface route in host ",
		zap.String("default overlay interface", defaultOverlayInterface),
		zap.Strings("underlay interface ips", chainedIPs))
	var err error
	// index of cali* or lxc* on host
	parentIndex := -1
	err = netNS.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(defaultOverlayInterface)
		if err != nil {
			logger.Error(err.Error())
			return err
		}
		parentIndex = link.Attrs().ParentIndex
		return nil
	})
	if err != nil {
		logger.Error(err.Error())
		return fmt.Errorf("failed to get parentIndex of %s in pod: %v", defaultOverlayInterface, err)
	}

	if parentIndex < 0 {
		return fmt.Errorf("parentIndex on found")
	}

	// debug: get overlay veth interface(cali* or lxc*)
	link, err := netlink.LinkByIndex(parentIndex)
	if err != nil {
		logger.Error(err.Error())
		return fmt.Errorf("failed to found default overlay veth interface: %v", err)
	}
	logger.Debug("found veth device of default-overlay cni on host", zap.String("Parent Device", link.Attrs().Name))

	hostIPs, err := utils.GetHostIps(logger, true, true)
	if err != nil {
		logger.Error(err.Error())
		return fmt.Errorf("failed to get host ips: %v", err)
	}
	for _, chainedIP := range chainedIPs {
		netIP, ipNet, err := net.ParseCIDR(chainedIP)
		if err != nil {
			logger.Error(err.Error())
			return err
		}
		for _, hostIP := range hostIPs {
			ip, _, err := net.ParseCIDR(hostIP)
			if err != nil {
				logger.Error(err.Error())
				return err
			}

			if ipNet.Contains(ip) {
				dst := &net.IPNet{
					IP: netIP,
				}
				var family int
				if netIP.To4() != nil {
					family = netlink.FAMILY_V4
					dst.Mask = net.CIDRMask(32, 32)
				} else {
					family = netlink.FAMILY_V6
					dst.Mask = net.CIDRMask(128, 128)
				}

				rule := netlink.NewRule()
				rule.Table = hostRuleTable
				rule.Family = family
				rule.Dst = dst
				if err = netlink.RuleAdd(rule); err != nil && !os.IsExist(err) {
					logger.Error("Netlink RuleAdd Failed", zap.String("Rule", rule.String()), zap.Error(err))
					return fmt.Errorf("failed to add rule table for underlay interface: %v", err)
				}

				if err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: parentIndex,
					Dst:       dst,
					Scope:     netlink.SCOPE_LINK,
					Table:     hostRuleTable,
				}); err != nil && !os.IsExist(err) {
					logger.Error(err.Error())
					return fmt.Errorf("failed to add route for underlay interface: %v", err)
				}
				logger.Debug("Succeed to add default overlay route on host", zap.Int("LinkIndex", parentIndex), zap.String("Dst", dst.String()))
				break
			}
		}
	}
	return nil
}
