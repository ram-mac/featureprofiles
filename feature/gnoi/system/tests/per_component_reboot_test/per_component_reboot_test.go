// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package per_component_reboot_test

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-traffic-generator/snappi/gosnappi"
	"github.com/openconfig/featureprofiles/internal/args"
	"github.com/openconfig/featureprofiles/internal/attrs"
	"github.com/openconfig/featureprofiles/internal/components"
	"github.com/openconfig/featureprofiles/internal/deviations"
	"github.com/openconfig/featureprofiles/internal/fptest"
	"github.com/openconfig/featureprofiles/internal/helpers"
	"github.com/openconfig/ondatra"
	"github.com/openconfig/ygnmi/ygnmi"
	"github.com/openconfig/ygot/ygot"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spb "github.com/openconfig/gnoi/system"
	tpb "github.com/openconfig/gnoi/types"
	"github.com/openconfig/ondatra/gnmi"
	"github.com/openconfig/ondatra/gnmi/oc"
)

const (
	controlcardType   = oc.PlatformTypes_OPENCONFIG_HARDWARE_COMPONENT_CONTROLLER_CARD
	linecardType      = oc.PlatformTypes_OPENCONFIG_HARDWARE_COMPONENT_LINECARD
	fabricType        = oc.PlatformTypes_OPENCONFIG_HARDWARE_COMPONENT_FABRIC
	activeController  = oc.Platform_ComponentRedundantRole_PRIMARY
	standbyController = oc.Platform_ComponentRedundantRole_SECONDARY
	ipv4PrefixLen     = 30
	flowPPS           = 500
	flowPacketSize    = 512
)

var (
	trapstatsRe = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+([\w\.\s]+)\s+(\d+)\s+(\d+)`)

	dutSrc = attrs.Attributes{
		Desc:    "dutSrc",
		IPv4:    "192.168.1.1",
		IPv4Len: ipv4PrefixLen,
	}
	ateSrc = attrs.Attributes{
		Name:    "ateSrc",
		IPv4:    "192.168.1.2",
		MAC:     "02:00:01:01:01:01",
		IPv4Len: ipv4PrefixLen,
	}
	dutDst = attrs.Attributes{
		Desc:    "dutDst",
		IPv4:    "192.168.1.5",
		IPv4Len: ipv4PrefixLen,
	}
	ateDst = attrs.Attributes{
		Name:    "ateDst",
		IPv4:    "192.168.1.6",
		MAC:     "02:00:02:01:01:01",
		IPv4Len: ipv4PrefixLen,
	}
)

func TestMain(m *testing.M) {
	fptest.RunTests(m)
}

// Test cases:
//  1) Issue gnoi.system Reboot to chassis with
//     - Delay: Not set.
//     - message: Not set.
//     - method: Only the COLD method is required to be supported by all targets.
//     - subcomponents: Standby RP/supervisor or linecard name.
//  2) Set the subcomponent to a standby RP (supervisor).
//     - Verify that the standby RP has rebooted and the uptime has been reset.
//  3) Set the subcomponent to a a field-removable linecard in the system.
//     - Verify that the line card has rebooted and the uptime has been reset.
//
// Topology:
//   DUT
//
// Test notes:
//  - Reboot causes the target to reboot, possibly at some point in the future.
//    If the method of reboot is not supported then the Reboot RPC will fail.
//    If the reboot is immediate the command will block until the subcomponents
//    have restarted.
//    If a reboot on the active control processor is pending the service must
//    reject all other reboot requests.
//    If a reboot request for active control processor is initiated with other
//    pending reboot requests it must be rejected.
//  - Only standby RP/supervisor reboot is tested
//    - Active RP/RP/supervisor reboot might not be supported for some platforms.
//    - Chassis reboot or RP switchover should be performed instead of active
//      RP/RP/supervisor reboot in real world.
//
//  - TODO: Check the uptime has been reset after the reboot.
//
//  - gnoi operation commands can be sent and tested using CLI command grpcurl.
//    https://github.com/fullstorydev/grpcurl
//

func TestStandbyControllerCardReboot(t *testing.T) {
	dut := ondatra.DUT(t, "dut")

	controllerCards := components.FindComponentsByType(t, dut, controlcardType)
	t.Logf("Found controller card list: %v", controllerCards)

	if *args.NumControllerCards >= 0 && len(controllerCards) != *args.NumControllerCards {
		t.Errorf("Incorrect number of controller cards: got %v, want exactly %v (specified by flag)", len(controllerCards), *args.NumControllerCards)
	}

	if got, want := len(controllerCards), 2; got < want {
		t.Skipf("Not enough controller cards for the test on %v: got %v, want at least %v", dut.Model(), got, want)
	}

	rpStandby, rpActive := components.FindStandbyControllerCard(t, dut, controllerCards)
	t.Logf("Detected rpStandby: %v, rpActive: %v", rpStandby, rpActive)

	gnoiClient := dut.RawAPIs().GNOI(t)
	useNameOnly := deviations.GNOISubcomponentPath(dut)
	rebootSubComponentRequest := &spb.RebootRequest{
		Method: spb.RebootMethod_COLD,
		Subcomponents: []*tpb.Path{
			components.GetSubcomponentPath(rpStandby, useNameOnly),
		},
	}

	t.Logf("rebootSubComponentRequest: %v", rebootSubComponentRequest)
	startReboot := time.Now()
	rebootResponse, err := gnoiClient.System().Reboot(context.Background(), rebootSubComponentRequest)
	if err != nil {
		t.Fatalf("Failed to perform component reboot with unexpected err: %v", err)
	}
	t.Logf("gnoiClient.System().Reboot() response: %v, err: %v", rebootResponse, err)

	t.Logf("Wait for a minute to allow the sub component's reboot process to start")
	time.Sleep(1 * time.Minute)

	watch := gnmi.Watch(t, dut, gnmi.OC().Component(rpStandby).RedundantRole().State(), 10*time.Minute, func(val *ygnmi.Value[oc.E_Platform_ComponentRedundantRole]) bool {
		return val.IsPresent()
	})
	if val, ok := watch.Await(t); !ok {
		t.Fatalf("DUT did not reach target state within %v: got %v", 10*time.Minute, val)
	}
	t.Logf("Standby controller boot time: %.2f seconds", time.Since(startReboot).Seconds())

	// TODO: Check the standby RP uptime has been reset.
}

// configInterfaceDUT configures the interface with the Addrs.
func configInterfaceDUT(i *oc.Interface, a *attrs.Attributes, dut *ondatra.DUTDevice) *oc.Interface {
	i.Description = ygot.String(a.Desc)
	i.Type = oc.IETFInterfaces_InterfaceType_ethernetCsmacd

	if deviations.InterfaceEnabled(dut) {
		i.Enabled = ygot.Bool(true)
	}

	s := i.GetOrCreateSubinterface(0)
	s4 := s.GetOrCreateIpv4()
	if deviations.InterfaceEnabled(dut) {
		s4.Enabled = ygot.Bool(true)
	}
	s4.GetOrCreateAddress(a.IPv4).PrefixLength = ygot.Uint8(ipv4PrefixLen)

	return i
}

// configureDUT configures port1, port2 on the DUT and enables the interfaces.
func configureDUT(t *testing.T, dut *ondatra.DUTDevice) {
	t.Helper()
	d := gnmi.OC()

	p1 := dut.Port(t, "port1")
	i1 := &oc.Interface{Name: ygot.String(p1.Name())}
	i1.Enabled = ygot.Bool(true)
	gnmi.Update(t, dut, d.Interface(p1.Name()).Config(), configInterfaceDUT(i1, &dutSrc, dut))

	p2 := dut.Port(t, "port2")
	i2 := &oc.Interface{Name: ygot.String(p2.Name())}
	i2.Enabled = ygot.Bool(true)
	gnmi.Update(t, dut, d.Interface(p2.Name()).Config(), configInterfaceDUT(i2, &dutDst, dut))
}

// configureOTG configures the OTG with the ateSrc and ateDst.
func configureOTG(t *testing.T, ate *ondatra.ATEDevice) gosnappi.Config {
	t.Helper()

	top := gosnappi.NewConfig()
	p1 := ate.Port(t, "port1")
	p2 := ate.Port(t, "port2")
	ateSrc.AddToOTG(top, p1, &dutSrc)
	ateDst.AddToOTG(top, p2, &dutDst)
	return top
}

// createTrafficFlows creates the traffic flows for each PBR policy.
func createTrafficFlows(t *testing.T, top gosnappi.Config, ate *ondatra.ATEDevice, dut *ondatra.DUTDevice) {
	t.Helper()

	flowName := "flow-ipv4"
	flow := top.Flows().Add().SetName(flowName)
	flow.TxRx().Port().
		SetTxName(ate.Port(t, "port1").ID()).
		SetRxNames([]string{ate.Port(t, "port2").ID()})

	flow.Metrics().SetEnable(true)
	flow.Rate().SetPps(flowPPS)
	flow.Size().SetFixed(flowPacketSize)
	flow.Duration().Continuous()

	eth := flow.Packet().Add().Ethernet()
	eth.Src().SetValue(ateSrc.MAC)
	dutDstInterface := dut.Port(t, "port1").Name()
	dstMac := gnmi.Get(t, dut, gnmi.OC().Interface(dutDstInterface).Ethernet().MacAddress().State())
	eth.Dst().SetValue(dstMac)

	ip := flow.Packet().Add().Ipv4()
	ip.Src().SetValue(ateSrc.IPv4)
	ip.Dst().SetValue(dutSrc.IPv4)
}

// trapStats represents a single row of trap statistics.
type trapStats struct {
	dev      int
	trapcode int
	name     string
	count    int
	rate     int
}

// parseTrapStats parses the output of the request pfe execute target fpc* command " show cda trapstats" | no-more command.
func parseTrapStats(t *testing.T, output string) ([]trapStats, error) {
	t.Helper()

	var stats []trapStats
	var parsingTable bool
	scanner := bufio.NewScanner(strings.NewReader(output))

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "DEV") {
			parsingTable = true
			continue
		}

		if !parsingTable {
			continue
		}

		match := trapstatsRe.FindStringSubmatch(line)
		if match == nil {
			if len(strings.TrimSpace(line)) > 0 {
				return nil, fmt.Errorf("invalid line format: %s", line)
			}
			continue
		}

		dev, err := strconv.Atoi(strings.TrimSpace(match[1]))
		if err != nil {
			return nil, fmt.Errorf("error parsing DEV: %w", err)
		}
		trapCode, err := strconv.Atoi(strings.TrimSpace(match[2]))
		if err != nil {
			return nil, fmt.Errorf("error parsing TRAPCODE: %w", err)
		}
		name := strings.TrimSpace(match[3])
		count, err := strconv.Atoi(strings.TrimSpace(match[4]))
		if err != nil {
			return nil, fmt.Errorf("error parsing COUNT: %w", err)
		}
		rate, err := strconv.Atoi(strings.TrimSpace(match[5]))
		if err != nil {
			return nil, fmt.Errorf("error parsing RATE: %w", err)
		}

		stats = append(stats, trapStats{
			dev:      dev,
			trapcode: trapCode,
			name:     name,
			count:    count,
			rate:     rate,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading output: %w", err)
	}

	return stats, nil
}

func testTrafficDrop(t *testing.T, dut *ondatra.DUTDevice, linecard string) {
	// TODO: Add traffic drop check for other vendors
	if dut.Vendor() != ondatra.JUNIPER {
		return
	}
	t.Log("Configure DUT")
	configureDUT(t, dut)
	t.Log("Configure OTG")
	ate := ondatra.ATE(t, "ate")
	top := configureOTG(t, ate)
	createTrafficFlows(t, top, ate, dut)

	t.Log("Push config to the OTG device")
	t.Log(top.String())
	otgObj := ate.OTG()
	otgObj.PushConfig(t, top)

	incomingPort := "port1"
	initialCounters := gnmi.Get(t, dut, gnmi.OC().Interface(dut.Port(t, incomingPort).Name()).Counters().State())
	initialInPkts := initialCounters.GetInPkts()
	t.Logf("initial incoming packets: %v", initialInPkts)

	t.Log("Start protocols and traffic")
	otgObj.StartProtocols(t)
	otgObj.StartTraffic(t)

	command := fmt.Sprintf("request pfe execute target %s command \"show cda trapstats\" | no-more", linecard)
	for idx := 0; idx < 10; idx++ {
		time.Sleep(30 * time.Second)
		result := dut.CLI().RunResult(t, command)
		if result.Error() != "" {
			t.Errorf("could not fetch output for: %s, err: %s", command, result.Error())
			break
		}
		stats, err := parseTrapStats(t, result.Output())
		if err != nil {
			t.Errorf("could not parse output for: %s, output:\n%s \nerr: %s", command, result.Output(), err)
			break
		}

		for i := range stats {
			stat := &stats[i]
			if stat.rate != 0 {
				t.Errorf("found non-zero rate for stat: %s, rate: %d", stat.name, stat.rate)
			}
		}
	}
	t.Log("Stop traffic")
	otgObj.StopTraffic(t)
	t.Log("Stop protocols")
	otgObj.StopProtocols(t)

	finalCounters := gnmi.Get(t, dut, gnmi.OC().Interface(dut.Port(t, incomingPort).Name()).Counters().State())
	finalInPkts := finalCounters.GetInPkts()
	t.Logf("final incoming packets: %v", finalInPkts)

	if finalInPkts == initialInPkts {
		t.Errorf("incoming packets did not change after traffic was started")
	}
}

// fpcFromPort extracts the FPC name from a Juniper port name.
func fpcFromPort(t testing.TB, portName string) (string, error) {
	t.Helper()
	re := regexp.MustCompile(`^[a-z]+-(\d+)/\d+/\d+(?::\d+)?$`)
	match := re.FindStringSubmatch(portName)
	if match == nil {
		return "", fmt.Errorf("invalid port name format: %s", portName)
	}
	return fmt.Sprintf("FPC%s", match[1]), nil
}

func TestLinecardReboot(t *testing.T) {
	const linecardBoottime = 10 * time.Minute
	dut := ondatra.DUT(t, "dut")

	lcs := components.FindComponentsByType(t, dut, linecardType)
	t.Logf("Found linecard list: %v", lcs)

	var validCards []string
	// don't consider the empty linecard slots.
	if len(lcs) > *args.NumLinecards {
		for _, lc := range lcs {
			empty, ok := gnmi.Lookup(t, dut, gnmi.OC().Component(lc).Empty().State()).Val()
			if !ok || (ok && !empty) {
				validCards = append(validCards, lc)
			}
		}
	} else {
		validCards = lcs
	}
	if *args.NumLinecards >= 0 && len(validCards) != *args.NumLinecards {
		t.Errorf("Incorrect number of linecards: got %v, want exactly %v (specified by flag)", len(validCards), *args.NumLinecards)
	}

	if got := len(validCards); got == 0 {
		t.Skipf("Not enough linecards for the test on %v: got %v, want > 0", dut.Model(), got)
	}

	var lineCardToReboot string
	if dut.Vendor() == ondatra.JUNIPER {
		portName := dut.Port(t, "port1").Name()
		var err error
		lineCardToReboot, err = fpcFromPort(t, portName)
		if err != nil {
			t.Fatalf("Failed to get line card to reboot: %v", err)
		}
		t.Logf("line card to reboot: %v", lineCardToReboot)
	}

	var removableLinecard string
	for _, lc := range validCards {
		t.Logf("Check if %s is removable", lc)
		if dut.Vendor() == ondatra.JUNIPER && lc != lineCardToReboot {
			continue
		}
		if got := gnmi.Lookup(t, dut, gnmi.OC().Component(lc).Removable().State()).IsPresent(); !got {
			t.Logf("Detected non-removable line card: %v", lc)
			continue
		}
		if got := gnmi.Get(t, dut, gnmi.OC().Component(lc).Removable().State()); got {
			t.Logf("Found removable line card: %v", lc)
			removableLinecard = lc
		}
	}
	if removableLinecard == "" {
		if *args.NumLinecards > 0 {
			t.Fatalf("No removable line card found for the testing on a modular device")
		} else {
			t.Skipf("No removable line card found for the testing")
		}
	}

	gnoiClient := dut.RawAPIs().GNOI(t)
	useNameOnly := deviations.GNOISubcomponentPath(dut)
	rebootSubComponentRequest := &spb.RebootRequest{
		Method: spb.RebootMethod_COLD,
		Subcomponents: []*tpb.Path{
			components.GetSubcomponentPath(removableLinecard, useNameOnly),
		},
	}

	intfsOperStatusUPBeforeReboot := helpers.FetchOperStatusUPIntfs(t, dut, *args.CheckInterfacesInBinding)
	t.Logf("OperStatusUP interfaces before reboot: %v", intfsOperStatusUPBeforeReboot)
	t.Logf("rebootSubComponentRequest: %v", rebootSubComponentRequest)
	rebootResponse, err := gnoiClient.System().Reboot(context.Background(), rebootSubComponentRequest)
	if err != nil {
		t.Fatalf("Failed to perform line card reboot with unexpected err: %v", err)
	}
	t.Logf("gnoiClient.System().Reboot() response: %v, err: %v", rebootResponse, err)

	t.Logf("Wait for 10s to allow the sub component's reboot process to start")
	time.Sleep(10 * time.Second)

	req := &spb.RebootStatusRequest{
		Subcomponents: rebootSubComponentRequest.GetSubcomponents(),
	}

	if deviations.GNOISubcomponentRebootStatusUnsupported(dut) {
		req.Subcomponents = nil
	}
	rebootDeadline := time.Now().Add(linecardBoottime)
	for retry := true; retry; {
		t.Log("Waiting for 10 seconds before checking.")
		time.Sleep(10 * time.Second)
		if time.Now().After(rebootDeadline) {
			retry = false
			break
		}
		resp, err := gnoiClient.System().RebootStatus(context.Background(), req)
		switch {
		case status.Code(err) == codes.Unimplemented:
			t.Fatalf("Unimplemented RebootStatus() is not fully compliant with the Reboot spec.")
		case err == nil:
			retry = resp.GetActive()
		default:
			// any other error just sleep.
		}
	}

	t.Logf("Validate removable linecard %v status", removableLinecard)
	gnmi.Await(t, dut, gnmi.OC().Component(removableLinecard).Removable().State(), linecardBoottime, true)

	helpers.ValidateOperStatusUPIntfs(t, dut, intfsOperStatusUPBeforeReboot, 10*time.Minute)
	// TODO: Check the line card uptime has been reset.
	testTrafficDrop(t, dut, strings.ToLower(removableLinecard))
}

// Reboot the fabric component on the DUT.
func TestFabricReboot(t *testing.T) {
	dut := ondatra.DUT(t, "dut")
	if deviations.GNOIFabricComponentRebootUnsupported(dut) {
		t.Skipf("Skipping test due to deviation deviation_gnoi_fabric_component_reboot_unsupported")
	}

	const fabricBootTime = 10 * time.Minute
	fabrics := components.FindComponentsByType(t, dut, fabricType)
	t.Logf("Found fabric components: %v", fabrics)

	if *args.NumFabrics >= 0 && len(fabrics) != *args.NumFabrics {
		t.Errorf("Incorrect number of fabrics: got %v, want exactly %v (specified by flag)", len(fabrics), *args.NumFabrics)
	}

	var removableFabric string
	for _, fabric := range fabrics {
		t.Logf("Check if %s is removable", fabric)
		if removable, ok := gnmi.Lookup(t, dut, gnmi.OC().Component(fabric).Removable().State()).Val(); ok && removable {
			t.Logf("Found removable fabric component: %v", fabric)
			removableFabric = fabric
			break
		} else {
			t.Logf("Found non-removable fabric component: %v", fabric)
		}
	}
	if removableFabric == "" {
		if *args.NumFabrics > 0 {
			t.Fatalf("No removable fabric component found for the testing on a modular device")
		} else {
			t.Skipf("No removable fabric component found for the testing")
		}
	}

	// Fetch list of interfaces which are up prior to fabric component reboot.
	intfsOperStatusUPBeforeReboot := helpers.FetchOperStatusUPIntfs(t, dut, *args.CheckInterfacesInBinding)
	t.Logf("OperStatusUP interfaces before reboot: %v", intfsOperStatusUPBeforeReboot)

	// Fetch a new gnoi client.
	gnoiClient := dut.RawAPIs().GNOI(t)
	useNameOnly := deviations.GNOISubcomponentPath(dut)
	rebootSubComponentRequest := &spb.RebootRequest{
		Method: spb.RebootMethod_COLD,
		Subcomponents: []*tpb.Path{
			components.GetSubcomponentPath(removableFabric, useNameOnly),
		},
	}

	t.Logf("rebootSubComponentRequest: %v", rebootSubComponentRequest)
	rebootResponse, err := gnoiClient.System().Reboot(context.Background(), rebootSubComponentRequest)
	if err != nil {
		t.Fatalf("Failed to perform fabric component reboot with unexpected err: %v", err)
	}
	t.Logf("gnoiClient.System().Reboot() response: %v, err: %v", rebootResponse, err)

	req := &spb.RebootStatusRequest{
		Subcomponents: rebootSubComponentRequest.GetSubcomponents(),
	}

	if deviations.GNOISubcomponentRebootStatusUnsupported(dut) {
		req.Subcomponents = nil
	}
	rebootDeadline := time.Now().Add(fabricBootTime)
	for {
		t.Log("Waiting for 10 seconds before checking.")
		time.Sleep(10 * time.Second)
		if time.Now().After(rebootDeadline) {
			break
		}
		resp, err := gnoiClient.System().RebootStatus(context.Background(), req)
		if status.Code(err) == codes.Unimplemented {
			t.Fatalf("Unimplemented RebootStatus() is not fully compliant with the Reboot spec.")
		}
		if !resp.GetActive() {
			break
		}
	}

	// Wait for the fabric component to come back up.
	t.Logf("Validate removable fabric component %v status", removableFabric)
	gnmi.Await(t, dut, gnmi.OC().Component(removableFabric).OperStatus().State(), fabricBootTime, oc.PlatformTypes_COMPONENT_OPER_STATUS_ACTIVE)
	t.Logf("Fabric component is active")
	helpers.ValidateOperStatusUPIntfs(t, dut, intfsOperStatusUPBeforeReboot, 5*time.Minute)
	// TODO: Check the fabric component uptime has been reset.
}
