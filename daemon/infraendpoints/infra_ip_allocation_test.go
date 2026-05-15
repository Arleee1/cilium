// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package infraendpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/cilium/hive/hivetest"
	cniInvoke "github.com/containernetworking/cni/pkg/invoke"
	cniTypes "github.com/containernetworking/cni/pkg/types"
	cniTypesV1 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	fakecni "github.com/cilium/cilium/daemon/cmd/cni/fake"
	"github.com/cilium/cilium/pkg/cidr"
	"github.com/cilium/cilium/pkg/datapath/linux/safenetlink"
	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/ipam"
	"github.com/cilium/cilium/pkg/mac"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/testutils"
	"github.com/cilium/cilium/pkg/testutils/netns"
	cnitypes "github.com/cilium/cilium/plugins/cilium-cni/types"
)

func TestCoalesceCIDRs(t *testing.T) {
	infraIPAllocator := &infraIPAllocator{
		logger: hivetest.Logger(t),
	}

	CIDR := []string{"10.0.0.0/8"}
	expectedCIDR := []string{"10.0.0.0/8"}
	newCIDR, err := infraIPAllocator.coalesceCIDRs(CIDR)
	if err != nil || len(newCIDR) != len(expectedCIDR) || newCIDR[0] != expectedCIDR[0] {
		t.Errorf("got %v, want %v, err: %v\n", newCIDR, expectedCIDR, err)
	}

	CIDR = []string{"10.105.0.0/16", "10.0.0.0/8"}
	expectedCIDR = []string{"10.0.0.0/8"}
	newCIDR, err = infraIPAllocator.coalesceCIDRs(CIDR)
	if err != nil || len(newCIDR) != len(expectedCIDR) || newCIDR[0] != expectedCIDR[0] {
		t.Errorf("got %v, want %v, err: %v\n", newCIDR, expectedCIDR, err)
	}

	CIDR = []string{"10.105.0.0/16", "10.104.0.0/19", "10.0.0.0/8"}
	expectedCIDR = []string{"10.0.0.0/8"}
	newCIDR, err = infraIPAllocator.coalesceCIDRs(CIDR)
	if err != nil || len(newCIDR) != len(expectedCIDR) || newCIDR[0] != expectedCIDR[0] {
		t.Errorf("got %v, want %v, err: %v\n", newCIDR, expectedCIDR, err)
	}

	CIDR = []string{"10.105.0.0/16", "192.168.1.0/24"}
	expectedCIDR = []string{"10.105.0.0/16", "192.168.1.0/24"}
	newCIDR, err = infraIPAllocator.coalesceCIDRs(CIDR)
	if err != nil || len(newCIDR) != len(expectedCIDR) || newCIDR[0] != expectedCIDR[0] || newCIDR[1] != expectedCIDR[1] {
		t.Errorf("got %v, want %v, err: %v\n", newCIDR, expectedCIDR, err)
	}

	CIDR = []string{"10.105.0.0/16", "192.168.1.0/24", "10.0.0.0/8"}
	expectedCIDR = []string{"10.0.0.0/8", "192.168.1.0/24"}
	newCIDR, err = infraIPAllocator.coalesceCIDRs(CIDR)
	if err != nil || len(newCIDR) != len(expectedCIDR) || newCIDR[0] != expectedCIDR[0] || newCIDR[1] != expectedCIDR[1] {
		t.Errorf("got %v, want %v, err: %v\n", newCIDR, expectedCIDR, err)
	}

	CIDR = []string{"10.105.0.0/16", "192.168.1.0/24", "10.0.0.0/8", "f00d::a0f:0:0:0/96"}
	expectedCIDR = []string{"10.0.0.0/8", "192.168.1.0/24", "f00d::a0f:0:0:0/96"}
	newCIDR, err = infraIPAllocator.coalesceCIDRs(CIDR)
	if err != nil || len(newCIDR) != len(expectedCIDR) || newCIDR[0] != expectedCIDR[0] || newCIDR[1] != expectedCIDR[1] || newCIDR[2] != expectedCIDR[2] {
		t.Errorf("got %v, want %v, err: %v\n", newCIDR, expectedCIDR, err)
	}

	CIDR = []string{"f00d::a0f:0:0:0/96", "10.105.0.0/16", "192.168.1.0/24", "10.0.0.0/8"}
	expectedCIDR = []string{"10.0.0.0/8", "192.168.1.0/24", "f00d::a0f:0:0:0/96"}
	newCIDR, err = infraIPAllocator.coalesceCIDRs(CIDR)
	if err != nil || len(newCIDR) != len(expectedCIDR) || newCIDR[0] != expectedCIDR[0] || newCIDR[1] != expectedCIDR[1] || newCIDR[2] != expectedCIDR[2] {
		t.Errorf("got %v, want %v, err: %v\n", newCIDR, expectedCIDR, err)
	}

	CIDR = []string{"f00d::a0f:0:0:0/96"}
	expectedCIDR = []string{"f00d::a0f:0:0:0/96"}
	newCIDR, err = infraIPAllocator.coalesceCIDRs(CIDR)
	if err != nil || len(newCIDR) != len(expectedCIDR) || newCIDR[0] != expectedCIDR[0] {
		t.Errorf("got %v, want %v, err: %v\n", newCIDR, expectedCIDR, err)
	}
}

type mockIPAllocator struct {
	allocCIDR *cidr.CIDR
}

func (m *mockIPAllocator) AllocateIPWithoutSyncUpstream(ip net.IP, owner string, pool ipam.Pool) (*ipam.AllocationResult, error) {
	if !m.allocCIDR.Contains(ip) {
		return nil, fmt.Errorf("cannot allocate IP %s", ip)
	}
	return &ipam.AllocationResult{IP: ip}, nil
}

func (m *mockIPAllocator) AllocateNextFamilyWithoutSyncUpstream(family ipam.Family, owner string, pool ipam.Pool) (result *ipam.AllocationResult, err error) {
	return nil, nil
}

func (m *mockIPAllocator) ExcludeIP(ip net.IP, owner string, pool ipam.Pool) {}

func (m *mockIPAllocator) ReleaseIP(ip net.IP, pool ipam.Pool) error {
	return nil
}

var _ ipamAllocator = &mockIPAllocator{}

func TestDaemon_reallocateDatapathIPs(t *testing.T) {
	infraIPAllocator := &infraIPAllocator{
		logger: hivetest.Logger(t),
		ipAllocator: &mockIPAllocator{
			allocCIDR: cidr.MustParseCIDR("10.20.30.0/24"),
		},
	}

	fromFS := net.ParseIP("10.20.30.42")
	fromK8s := net.ParseIP("10.20.30.41")

	invalidFromFS := net.ParseIP("172.16.0.42")
	invalidFromK8s := net.ParseIP("172.16.0.41")

	// no restoration needed
	result := infraIPAllocator.reallocateOldRouterIPs(nil, nil)
	assert.Nil(t, result)

	// fromK8s if fromFS is not available
	result = infraIPAllocator.reallocateOldRouterIPs(fromK8s, nil)
	assert.NotNil(t, result)
	assert.Equal(t, result.IP, fromK8s)

	// fromFS if fromK8s is not available
	result = infraIPAllocator.reallocateOldRouterIPs(nil, fromFS)
	assert.NotNil(t, result)
	assert.Equal(t, result.IP, fromFS)

	// fromFS should be preferred
	result = infraIPAllocator.reallocateOldRouterIPs(fromK8s, fromFS)
	assert.NotNil(t, result)
	assert.Equal(t, result.IP, fromFS)

	// reject restoration if the IP is not in the allocation CIDR
	result = infraIPAllocator.reallocateOldRouterIPs(invalidFromFS, invalidFromK8s)
	assert.Nil(t, result)

	// fromFS with invalid fromK8s
	result = infraIPAllocator.reallocateOldRouterIPs(invalidFromK8s, fromFS)
	assert.NotNil(t, result)
	assert.Equal(t, result.IP, fromFS)

	// fromFS with invalid fromK8s
	result = infraIPAllocator.reallocateOldRouterIPs(fromK8s, invalidFromFS)
	assert.NotNil(t, result)
	assert.Equal(t, result.IP, fromK8s)
}

func TestPrivilegedRemoveOldRouterState(t *testing.T) {
	testutils.PrivilegedTest(t)

	infraIPAllocator := &infraIPAllocator{
		logger: hivetest.Logger(t),
	}

	t.Run("test-1", func(t *testing.T) {
		ns := netns.NewNetNS(t)

		ns.Do(func() error {
			createDevices(t)

			// Assert that the old router IP (192.0.2.1) was removed because we are
			// restoring a different one (10.0.0.1).
			assert.NoError(t, infraIPAllocator.removeOldRouterState(false, net.ParseIP("10.0.0.1")))
			addrs, err := netlink.AddrList(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: defaults.HostDevice,
				},
			}, netlink.FAMILY_V4)
			assert.NoError(t, err)
			assert.Empty(t, addrs)

			// Assert no errors in the case we have no IPs to remove from cilium_host.
			assert.NoError(t, infraIPAllocator.removeOldRouterState(false, nil))

			return nil
		})
	})

	t.Run("test-2", func(t *testing.T) {
		ns := netns.NewNetNS(t)

		ns.Do(func() error {
			createDevices(t)

			// Remove the cilium_host device and assert no error on "link not found"
			// error.
			link, err := safenetlink.LinkByName(defaults.HostDevice)
			assert.NoError(t, err)
			assert.NotNil(t, link)
			assert.NoError(t, netlink.LinkDel(link))
			assert.NoError(t, infraIPAllocator.removeOldRouterState(false, nil))

			return nil
		})
	})
}

// createDevices creates the necessary devices for this test suite. Assumes it
// is executing within the new network namespace.
func createDevices(t *testing.T) {
	t.Helper()

	hostMac, err := mac.GenerateRandMAC()
	if err != nil {
		assert.NoError(t, err)
	}
	veth := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name:         defaults.HostDevice,
			HardwareAddr: net.HardwareAddr(hostMac),
			TxQLen:       1000,
		},
	}
	if err := netlink.LinkAdd(veth); err != nil {
		assert.NoError(t, err)
	}
	ciliumHost, err := safenetlink.LinkByName(defaults.HostDevice)
	if err != nil {
		assert.NoError(t, err)
	}

	_, ipnet, _ := net.ParseCIDR("192.0.2.1/32")
	addr := &netlink.Addr{IPNet: ipnet}
	assert.NoError(t, netlink.AddrAdd(ciliumHost, addr))
}

func Test_getCiliumHostIPsFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	allIPsCorrect := filepath.Join(tmpDir, "node_config.h")
	f, err := os.Create(allIPsCorrect)
	defer func(f *os.File) {
		require.NoError(t, f.Close())
	}(f)
	require.NoError(t, err)
	fmt.Fprintf(f, `/*
 cilium.v6.external.str fd01::b
 cilium.v6.internal.str f00d::a00:0:0:a4ad
 cilium.v6.nodeport.str []

 cilium.v4.external.str 192.168.60.11
 cilium.v4.internal.str 10.0.0.2
 cilium.v4.nodeport.str []

 cilium.v6.internal.raw 0xf0, 0xd, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0xa, 0x0, 0x0, 0x0, 0x0, 0x0, 0xa4, 0xad
 cilium.v4.internal.raw 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0xff, 0xff, 0xa, 0x0, 0x0, 0x2
 */

#define ENABLE_IPV4 1
#define IPV4_GATEWAY 0x100000a
#define HOST_IP 0xfd, 0x1, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0xb
#define HOST_ID 1
#define WORLD_ID 2
#define CILIUM_LB_MAP_MAX_ENTRIES 65536
#define ENDPOINTS_MAP_SIZE 65535
#define LPM_MAP_SIZE 16384
#define POLICY_MAP_SIZE 16384
#define IPCACHE_MAP_SIZE 512000
#define POLICY_PROG_MAP_SIZE 65535
#define TRACE_PAYLOAD_LEN 128ULL
#ifndef CILIUM_NET_MAC
#define CILIUM_NET_MAC { .addr = {0x26,0x11,0x70,0xcc,0xca,0x0c}}
#endif /* CILIUM_NET_MAC */
#define CILIUM_NET_IFINDEX 356
#define CILIUM_HOST_MAC { .addr = {0x3e,0x28,0xb4,0x4b,0x95,0x25}}
#define ENCAP_IFINDEX 358
`)

	type args struct {
		nodeConfig string
	}
	tests := []struct {
		name            string
		args            args
		wantIpv4GW      net.IP
		wantIpv6Router  net.IP
		wantIpv6Address net.IP
	}{
		{
			name: "every-ip-correct",
			args: args{
				nodeConfig: allIPsCorrect,
			},
			wantIpv4GW:      net.ParseIP("10.0.0.2"),
			wantIpv6Router:  net.ParseIP("f00d::a00:0:0:a4ad"),
			wantIpv6Address: net.ParseIP("fd01::b"),
		},
		{
			name: "file-not-present",
			args: args{
				nodeConfig: "",
			},
			wantIpv4GW:     nil,
			wantIpv6Router: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIpv4GW, gotIpv6Router := getCiliumHostIPsFromFile(tt.args.nodeConfig)
			require.Equal(t, tt.wantIpv4GW, gotIpv4GW)
			require.Equal(t, tt.wantIpv6Router, gotIpv6Router)
		})
	}
}

func TestNewDelegatedIPAMArgs(t *testing.T) {
	nodeName := "test-node"
	cniBinPath := "/opt/cni/bin"

	args := newDelegatedIPAMArgs("ADD", nodeName, cniBinPath)

	assert.Equal(t, "ADD", args.Command)
	assert.Equal(t, "cilium-ingress-test-node", args.ContainerID)
	assert.Equal(t, "/proc/1/ns/net", args.NetNS)
	assert.Equal(t, "eth0", args.IfName)
	assert.Equal(t, cniBinPath, args.Path)
	assert.Len(t, args.PluginArgs, 2)
	assert.Equal(t, [2]string{"K8S_POD_NAME", "cilium-ingress-test-node"}, args.PluginArgs[0])
	assert.Equal(t, [2]string{"K8S_POD_NAMESPACE", "kube-system"}, args.PluginArgs[1])
}

func TestDelegatedIPAMArgsAsEnv(t *testing.T) {
	args := newDelegatedIPAMArgs("ADD", "test-node", "/opt/cni/bin")
	env := args.AsEnv()

	// Check that CNI_NETNS_OVERRIDE=1 is appended
	found := false
	for _, e := range env {
		if e == "CNI_NETNS_OVERRIDE=1" {
			found = true
			break
		}
	}
	assert.True(t, found, "CNI_NETNS_OVERRIDE=1 should be in environment")

	// Verify standard CNI env vars are present
	envMap := make(map[string]bool)
	for _, e := range env {
		envMap[e] = true
	}
	assert.True(t, envMap["CNI_COMMAND=ADD"])
	assert.True(t, envMap["CNI_CONTAINERID=cilium-ingress-test-node"])
	assert.True(t, envMap["CNI_NETNS=/proc/1/ns/net"])
	assert.True(t, envMap["CNI_IFNAME=eth0"])
	assert.True(t, envMap["CNI_PATH=/opt/cni/bin"])
}

// mockCNIExec implements cniInvoke.Exec for testing.
type mockCNIExec struct {
	findInPathFn func(plugin string, paths []string) (string, error)
	execPluginFn func(ctx context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error)
}

func (m *mockCNIExec) FindInPath(plugin string, paths []string) (string, error) {
	if m.findInPathFn != nil {
		return m.findInPathFn(plugin, paths)
	}
	return "/opt/cni/bin/" + plugin, nil
}

func (m *mockCNIExec) ExecPlugin(ctx context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
	if m.execPluginFn != nil {
		return m.execPluginFn(ctx, pluginPath, stdinData, environ)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCNIExec) Decode(jsonBytes []byte) (version.PluginInfo, error) {
	return version.PluginSupports("1.0.0"), nil
}

// Ensure mockCNIExec implements cniInvoke.Exec
var _ cniInvoke.Exec = (*mockCNIExec)(nil)

// mockResultToExecFn creates a mock exec function that returns the given IPs.
func mockResultToExecFn(ips []net.IP) func(ctx context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
	return func(ctx context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
		var ipConfigs []*cniTypesV1.IPConfig
		for _, ip := range ips {
			mask := net.CIDRMask(24, 32)
			if ip.To4() == nil {
				mask = net.CIDRMask(128, 128)
			}
			ipConfigs = append(ipConfigs, &cniTypesV1.IPConfig{
				Address: net.IPNet{IP: ip, Mask: mask},
			})
		}
		mockResult := &cniTypesV1.Result{CNIVersion: "1.0.0", IPs: ipConfigs}
		return json.Marshal(mockResult)
	}
}

// validNetConf returns a valid CNI config for testing.
func validNetConf() *cnitypes.NetConf {
	return &cnitypes.NetConf{
		NetConf: cniTypes.NetConf{CNIVersion: "1.0.0", Name: "cilium"},
		IPAM:    cnitypes.IPAM{IPAM: cniTypes.IPAM{Type: "host-local"}},
	}
}

func TestAllocateIngressIPsWithDelegatedIPAM(t *testing.T) {
	tests := []struct {
		name           string
		enableIPv4     bool
		enableIPv6     bool
		existingIPv4   net.IP // pre-existing IP (triggers early return)
		existingIPv6   net.IP // pre-existing IP (triggers early return)
		expectedIPv4   net.IP
		expectedIPv6   net.IP
		expectedErrStr string
		netConf        *cnitypes.NetConf
		mock           *mockCNIExec
	}{
		{
			name:         "IPv4 only success",
			enableIPv4:   true,
			expectedIPv4: net.ParseIP("10.0.0.42"),
			netConf:      validNetConf(),
			mock:         &mockCNIExec{execPluginFn: mockResultToExecFn([]net.IP{net.ParseIP("10.0.0.42")})},
		},
		{
			name:         "IPv6 only success",
			enableIPv6:   true,
			expectedIPv6: net.ParseIP("fd00::42"),
			netConf:      validNetConf(),
			mock:         &mockCNIExec{execPluginFn: mockResultToExecFn([]net.IP{net.ParseIP("fd00::42")})},
		},
		{
			name:         "dual stack success",
			enableIPv4:   true,
			enableIPv6:   true,
			expectedIPv4: net.ParseIP("10.0.0.42"),
			expectedIPv6: net.ParseIP("fd00::42"),
			netConf:      validNetConf(),
			mock:         &mockCNIExec{execPluginFn: mockResultToExecFn([]net.IP{net.ParseIP("10.0.0.42"), net.ParseIP("fd00::42")})},
		},
		{
			name:           "CNI plugin fails",
			enableIPv4:     true,
			expectedErrStr: "delegated IPAM failed",
			netConf:        validNetConf(),
			mock: &mockCNIExec{
				execPluginFn: func(context.Context, string, []byte, []string) ([]byte, error) {
					return nil, errors.New("error from mock")
				},
			},
		},
		{
			name:           "missing IPv6 when required",
			enableIPv6:     true,
			expectedErrStr: "delegated IPAM did not return an IPv6 address",
			netConf:        validNetConf(),
			// Only IPv4 returned when IPv6 required
			mock: &mockCNIExec{execPluginFn: mockResultToExecFn([]net.IP{net.ParseIP("10.0.0.42")})},
		},
		{
			name:           "missing IPv4 in dual stack",
			enableIPv4:     true,
			enableIPv6:     true,
			expectedErrStr: "delegated IPAM did not return an IPv4 address",
			netConf:        validNetConf(),
			// Only IPv6 returned when both required
			mock: &mockCNIExec{execPluginFn: mockResultToExecFn([]net.IP{net.ParseIP("fd00::42")})},
		},
		{
			name:         "IPv4 already allocated",
			enableIPv4:   true,
			existingIPv4: net.ParseIP("10.0.0.100"),
			expectedIPv4: net.ParseIP("10.0.0.100"),
			mock:         &mockCNIExec{}, // not called - early return
		},
		{
			name:         "IPv6 already allocated",
			enableIPv6:   true,
			existingIPv6: net.ParseIP("fd00::100"),
			expectedIPv6: net.ParseIP("fd00::100"),
			mock:         &mockCNIExec{}, // not called - early return
		},
		// Validation error cases
		{
			name:           "nil CNI config",
			enableIPv4:     true,
			expectedErrStr: "CNI configuration not available",
			netConf:        nil,
			mock:           &mockCNIExec{},
		},
		{
			name:           "missing network name",
			enableIPv4:     true,
			expectedErrStr: "CNI config missing network name",
			netConf: &cnitypes.NetConf{
				NetConf: cniTypes.NetConf{Name: ""},
				IPAM:    cnitypes.IPAM{IPAM: cniTypes.IPAM{Type: "host-local"}},
			},
			mock: &mockCNIExec{},
		},
		{
			name:           "missing IPAM type",
			enableIPv4:     true,
			expectedErrStr: "CNI config missing IPAM type",
			netConf: &cnitypes.NetConf{
				NetConf: cniTypes.NetConf{Name: "cilium"},
				IPAM:    cnitypes.IPAM{IPAM: cniTypes.IPAM{Type: ""}},
			},
			mock: &mockCNIExec{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger := hivetest.Logger(t)

			localNode := node.LocalNode{}
			localNode.IPv4IngressIP = tc.existingIPv4
			localNode.IPv6IngressIP = tc.existingIPv6
			store := node.NewTestLocalNodeStore(localNode)

			fake := &fakecni.FakeCNIConfigManager{
				CustomNetConf:          tc.netConf,
				DelegatedIPAMCNIBinDir: "/opt/cni/bin",
			}

			r := &infraIPAllocator{
				logger:           logger,
				localNodeStore:   store,
				cniConfigManager: fake,
				daemonConfig: &option.DaemonConfig{
					EnableIPv4: tc.enableIPv4,
					EnableIPv6: tc.enableIPv6,
				},
			}

			err := r.allocateIngressIPsWithDelegatedIPAM(tc.mock)

			if tc.expectedErrStr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErrStr)
			} else {
				assert.NoError(t, err)
				updatedNode, _ := store.Get(context.Background())
				if tc.expectedIPv4 != nil {
					assert.NotNil(t, updatedNode.IPv4IngressIP)
					assert.True(t, tc.expectedIPv4.Equal(updatedNode.IPv4IngressIP), "expected %s, got %s", tc.expectedIPv4, updatedNode.IPv4IngressIP)
				}
				if tc.expectedIPv6 != nil {
					assert.NotNil(t, updatedNode.IPv6IngressIP)
					assert.True(t, tc.expectedIPv6.Equal(updatedNode.IPv6IngressIP), "expected %s, got %s", tc.expectedIPv6, updatedNode.IPv6IngressIP)
				}
			}
		})
	}
}

func TestDeallocateIngressIPsWithDelegatedIPAM(t *testing.T) {
	tests := []struct {
		name           string
		expectedErrStr string
		netConf        *cnitypes.NetConf
		mock           *mockCNIExec
	}{
		{
			name:    "success",
			netConf: validNetConf(),
			mock: &mockCNIExec{
				execPluginFn: func(context.Context, string, []byte, []string) ([]byte, error) { return nil, nil },
			},
		},
		{
			name:           "CNI plugin fails",
			expectedErrStr: "delegated IPAM deallocation failed",
			netConf:        validNetConf(),
			mock: &mockCNIExec{
				execPluginFn: func(context.Context, string, []byte, []string) ([]byte, error) {
					return nil, errors.New("error from mock")
				},
			},
		},
		{
			name:           "find plugin fails",
			expectedErrStr: "failed to find IPAM plugin",
			netConf:        validNetConf(),
			mock: &mockCNIExec{
				findInPathFn: func(string, []string) (string, error) { return "", errors.New("error from mock") },
			},
		},
		// Validation error cases
		{
			name:           "nil CNI config",
			expectedErrStr: "CNI configuration not available",
			netConf:        nil,
			mock:           &mockCNIExec{},
		},
		{
			name:           "missing network name",
			expectedErrStr: "CNI config missing network name",
			netConf: &cnitypes.NetConf{
				NetConf: cniTypes.NetConf{Name: ""},
				IPAM:    cnitypes.IPAM{IPAM: cniTypes.IPAM{Type: "host-local"}},
			},
			mock: &mockCNIExec{},
		},
		{
			name:           "missing IPAM type",
			expectedErrStr: "CNI config missing IPAM type",
			netConf: &cnitypes.NetConf{
				NetConf: cniTypes.NetConf{Name: "cilium"},
				IPAM:    cnitypes.IPAM{IPAM: cniTypes.IPAM{Type: ""}},
			},
			mock: &mockCNIExec{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger := hivetest.Logger(t)

			store := node.NewTestLocalNodeStore(node.LocalNode{})

			fake := &fakecni.FakeCNIConfigManager{
				CustomNetConf:          tc.netConf,
				DelegatedIPAMCNIBinDir: "/opt/cni/bin",
			}

			r := &infraIPAllocator{
				logger:           logger,
				localNodeStore:   store,
				cniConfigManager: fake,
				daemonConfig:     &option.DaemonConfig{},
			}

			err := r.deallocateIngressIPsWithDelegatedIPAM(tc.mock)

			if tc.expectedErrStr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErrStr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
