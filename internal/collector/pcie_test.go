package collector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mrlhansen/idrac_exporter/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// fakeBMC serves Redfish fixtures from testdata and records every request.
type fakeBMC struct {
	t        *testing.T
	server   *httptest.Server
	data     map[string]json.RawMessage
	override map[string]int // path -> forced HTTP status
	mu       sync.Mutex
	methods  map[string]int
	requests map[string]int
}

func newFakeBMC(t *testing.T, vendor string) *fakeBMC {
	t.Helper()

	raw, err := os.ReadFile("testdata/pcie_" + vendor + ".json")
	if err != nil {
		t.Fatal(err)
	}

	f := &fakeBMC{
		t:        t,
		override: map[string]int{},
		methods:  map[string]int{},
		requests: map[string]int{},
	}
	if err := json.Unmarshal(raw, &f.data); err != nil {
		t.Fatal(err)
	}

	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.methods[r.Method]++
		f.requests[r.URL.Path]++
		status, forced := f.override[r.URL.Path]
		f.mu.Unlock()

		if forced {
			w.WriteHeader(status)
			return
		}
		body, ok := f.data[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeBMC) client() *Client {
	config.Config = &config.RootConfig{Timeout: 5, Concurrency: 4, MetricsPrefix: "idrac"}
	host := strings.TrimPrefix(f.server.URL, "https://")
	auth := &config.AuthConfig{Scheme: "https", Username: "user", Password: "pass", BasicAuth: true}
	return &Client{
		redfish: NewRedfish(host, auth),
		metrics: &config.MetricsConfig{},
	}
}

func (f *fakeBMC) assertOnlyGet() {
	f.t.Helper()
	for m, n := range f.methods {
		if m != http.MethodGet {
			f.t.Errorf("unexpected %s requests: %d", m, n)
		}
	}
}

type series struct {
	labels map[string]string
	value  float64
}

// collectPCIe runs RefreshPCIeDevices and returns the emitted series by metric
// name and device id.
func collectPCIe(t *testing.T, client *Client) (map[string]map[string]series, bool) {
	t.Helper()
	mc := NewCollector()
	ch := make(chan prometheus.Metric, 256)
	ok := client.RefreshPCIeDevices(mc, ch)
	close(ch)

	out := map[string]map[string]series{"info": {}, "health": {}}
	for m := range ch {
		pb := &dto.Metric{}
		if err := m.Write(pb); err != nil {
			t.Fatal(err)
		}
		s := series{labels: map[string]string{}}
		for _, l := range pb.Label {
			s.labels[l.GetName()] = l.GetValue()
		}
		kind := "health"
		if m.Desc() == mc.PCIeDeviceInfo {
			kind = "info"
			s.value = pb.GetUntyped().GetValue()
		} else {
			s.value = pb.GetGauge().GetValue()
		}
		id := s.labels["id"]
		if _, dup := out[kind][id]; dup {
			t.Errorf("duplicate %s series for id %q", kind, id)
		}
		out[kind][id] = s
	}
	return out, ok
}

func discover(t *testing.T, f *fakeBMC, client *Client, system, primary string) {
	t.Helper()
	group := GroupResponse{}
	chassis := ChassisResponse{}
	if !client.redfish.Get("/redfish/v1/Chassis", &group) || !client.redfish.Get(primary, &chassis) {
		t.Fatal("failed to read chassis fixtures")
	}
	client.path.System = system
	client.path.PCIeDevices = client.findPCIeCollections(&group, primary, &chassis)
}

func TestPCIeLenovo(t *testing.T) {
	f := newFakeBMC(t, "lenovo")
	client := f.client()
	discover(t, f, client, "/redfish/v1/Systems/1", "/redfish/v1/Chassis/1")

	if got := client.path.PCIeDevices; len(got) != 1 || got[0] != "/redfish/v1/Chassis/1/PCIeDevices" {
		t.Fatalf("unexpected collections: %v", got)
	}

	res, ok := collectPCIe(t, client)
	if !ok {
		t.Error("expected scrape to succeed")
	}

	if _, found := res["info"]["ob_1"]; found {
		t.Error("ob_1 has no inventory data and must be skipped")
	}
	if len(res["info"]) != 3 || len(res["health"]) != 3 {
		t.Fatalf("expected 3 devices, got info=%d health=%d", len(res["info"]), len(res["health"]))
	}

	gpu := res["info"]["slot_3"].labels
	want := map[string]string{
		"id": "slot_3", "name": "NVIDIA L40S", "manufacturer": "NVIDIA", "model": "NVIDIA L40S",
		"part_number": "26B9-896-A1", "serial": "TESTSN0002", "slot": "PCIe 3", "firmware": "95.02.66.00.02",
	}
	for k, v := range want {
		if gpu[k] != v {
			t.Errorf("slot_3 label %s = %q, want %q", k, gpu[k], v)
		}
	}

	riser := res["info"]["slot_30"].labels
	for _, k := range []string{"model", "part_number", "serial"} {
		if riser[k] != "" {
			t.Errorf("slot_30 label %s = %q, want empty (N/A)", k, riser[k])
		}
	}
	if riser["manufacturer"] != "Lenovo" || riser["slot"] != "PCIe 30" {
		t.Errorf("unexpected slot_30 labels: %v", riser)
	}

	if h := res["health"]["slot_3"]; h.value != 0 || h.labels["status"] != "OK" {
		t.Errorf("unexpected slot_3 health: %+v", h)
	}

	// Each device is listed in the system and the chassis, but fetched once
	if n := f.requests["/redfish/v1/Chassis/1/PCIeDevices/slot_3"]; n != 1 {
		t.Errorf("slot_3 fetched %d times, want 1", n)
	}
	f.assertOnlyGet()
}

func TestPCIeDell(t *testing.T) {
	f := newFakeBMC(t, "dell")
	client := f.client()
	discover(t, f, client, "/redfish/v1/Systems/System.Embedded.1", "/redfish/v1/Chassis/System.Embedded.1")

	if len(client.path.PCIeDevices) != 2 {
		t.Fatalf("expected 2 collections (incl. empty enclosure), got %v", client.path.PCIeDevices)
	}

	res, ok := collectPCIe(t, client)
	if !ok {
		t.Error("expected scrape to succeed; an empty collection is not an error")
	}
	if len(res["info"]) != 4 {
		t.Fatalf("expected 4 devices, got %d", len(res["info"]))
	}

	gpu := res["info"]["192-0"].labels
	want := map[string]string{
		"name": "GH100 [H100 SXM5 80GB]", "manufacturer": "NVIDIA Corporation", "model": "H100 80GB HBM3",
		"part_number": "699-2G520-0200-400", "serial": "TESTSN0004", "slot": "192-0", "firmware": "",
	}
	for k, v := range want {
		if gpu[k] != v {
			t.Errorf("192-0 label %s = %q, want %q", k, gpu[k], v)
		}
	}

	if s := res["info"]["158-0"].labels["serial"]; s != "TESTSN0003" {
		t.Errorf("trailing whitespace not trimmed: %q", s)
	}
	if s := res["info"]["103-0"].labels["serial"]; s != "" {
		t.Errorf("null serial should be empty, got %q", s)
	}
	f.assertOnlyGet()
}

func TestPCIePartialFailure(t *testing.T) {
	f := newFakeBMC(t, "dell")
	client := f.client()
	discover(t, f, client, "/redfish/v1/Systems/System.Embedded.1", "/redfish/v1/Chassis/System.Embedded.1")

	f.override["/redfish/v1/Chassis/System.Embedded.1/PCIeDevices/103-0"] = http.StatusInternalServerError
	f.override["/redfish/v1/Chassis/System.Embedded.1/PCIeDevices"] = http.StatusNotFound

	res, ok := collectPCIe(t, client)
	if ok {
		t.Error("expected failure to be reported")
	}
	// The devices are still discovered via the system links
	if len(res["info"]) != 3 {
		t.Errorf("expected 3 remaining devices, got %d", len(res["info"]))
	}
	if _, found := res["info"]["192-0"]; !found {
		t.Error("192-0 missing after unrelated failure")
	}
}

func TestPCIeFallbackCollection(t *testing.T) {
	f := newFakeBMC(t, "lenovo")
	client := f.client()

	// Primary chassis without a PCIeDevices link: the conventional path is used
	chassis := ChassisResponse{}
	group := GroupResponse{Members: OdataSlice{{OdataId: "/redfish/v1/Chassis/1"}, {OdataId: "/redfish/v1/Chassis/3"}}}
	got := client.findPCIeCollections(&group, "/redfish/v1/Chassis/1", &chassis)
	if len(got) != 1 || got[0] != "/redfish/v1/Chassis/1/PCIeDevices" {
		t.Errorf("unexpected fallback result: %v", got)
	}

	// Non-existing fallback path is dropped
	group = GroupResponse{Members: OdataSlice{{OdataId: "/redfish/v1/Chassis/3"}}}
	got = client.findPCIeCollections(&group, "/redfish/v1/Chassis/3", &ChassisResponse{})
	if len(got) != 0 {
		t.Errorf("expected no collections, got %v", got)
	}

	// Secondary chassis without a link are not probed
	if n := f.requests["/redfish/v1/Chassis/3/PCIeDevices"]; n != 1 {
		t.Errorf("fallback probed %d times for Chassis/3, want 1 (only as primary)", n)
	}
	f.assertOnlyGet()
}

func TestPCIeDeviceUnmarshal(t *testing.T) {
	tests := []struct {
		name       string
		json       string
		empty      bool
		slot       string
		serial     string
		health     string
		healthEmit bool
	}{
		{"all null", `{"Id":"ob_1","Name":"Adapter","Model":null,"Manufacturer":null,"PartNumber":null,"SerialNumber":null,"Status":{"Health":"OK"}}`, true, "ob_1", "", "OK", true},
		{"N/A values", `{"Id":"x","Manufacturer":"Lenovo","Model":"N/A","SerialNumber":" n/a ","Slot":{"Location":{"PartLocation":{"ServiceLabel":"N/A"}}}}`, false, "x", "", "", false},
		{"missing status", `{"Id":"x","SerialNumber":"123"}`, false, "x", "123", "", false},
		{"null status and slot", `{"Id":"x","SerialNumber":"123","Status":null,"Slot":null}`, false, "x", "123", "", false},
		{"numeric serial", `{"Id":"x","SerialNumber":42,"Slot":{"Location":{"PartLocation":{"ServiceLabel":"PCIe 1"}}}}`, false, "PCIe 1", "42", "", false},
		{"empty array value", `{"Id":"x","Model":[],"SerialNumber":"1"}`, false, "x", "1", "", false},
		{"warning", `{"Id":"x","SerialNumber":"1","Status":{"Health":"Warning"}}`, false, "x", "1", "Warning", true},
	}

	config.Config = &config.RootConfig{MetricsPrefix: "idrac"}
	mc := NewCollector()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := PCIeDevice{}
			if err := json.Unmarshal([]byte(tc.json), &d); err != nil {
				t.Fatal(err)
			}
			d.Normalize()
			if d.IsInventoryEmpty() != tc.empty {
				t.Errorf("IsInventoryEmpty = %v, want %v", d.IsInventoryEmpty(), tc.empty)
			}
			if d.GetSlot() != tc.slot {
				t.Errorf("GetSlot = %q, want %q", d.GetSlot(), tc.slot)
			}
			if d.SerialNumber.String() != tc.serial {
				t.Errorf("serial = %q, want %q", d.SerialNumber, tc.serial)
			}

			ch := make(chan prometheus.Metric, 1)
			mc.NewPCIeDeviceHealth(ch, &d)
			close(ch)
			m, emitted := <-ch
			if emitted != tc.healthEmit {
				t.Fatalf("health emitted = %v, want %v", emitted, tc.healthEmit)
			}
			if emitted {
				pb := &dto.Metric{}
				m.Write(pb)
				if want := float64(health2value(tc.health)); pb.GetGauge().GetValue() != want {
					t.Errorf("health value = %v, want %v", pb.GetGauge().GetValue(), want)
				}
			}
		})
	}
}

func TestUniquePCIeLinks(t *testing.T) {
	links := OdataSlice{{OdataId: "/a/1"}, {OdataId: "/a/2"}, {OdataId: "/a/1/"}, {OdataId: ""}, {OdataId: "/a/2"}}
	got := uniquePCIeLinks(links)
	if len(got) != 2 || got[0] != "/a/1" || got[1] != "/a/2" {
		t.Errorf("unexpected result: %v", got)
	}
}
