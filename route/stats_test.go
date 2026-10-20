package route

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"github.com/ReCasaOS/CasaOS-VirtManagement/service"
)

const (
	vmUUID   = "11111111-1111-1111-1111-111111111111"
	vmID     = "vm%3A" + vmUUID
	idleUUID = "22222222-2222-2222-2222-222222222222" // shut off, never ran
	nobody   = "vm%3A00000000-0000-0000-0000-000000000000"

	defaultRules = `[{"key":"cpu_high","enabled":true,"threshold":90,"minutes":10},{"key":"mem_high","enabled":true,"threshold":90,"minutes":5}]`
)

// statsSource is a libvirt that answers from a script: one running VM using
// 25 % of a core, one shut off.
type statsSource struct {
	mu  sync.Mutex
	cpu uint64
	err error
}

func ptr[T any](v T) *T { return &v }

func (f *statsSource) Snapshot() ([]libvirt.DomainStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.cpu += 1.25e9
	return []libvirt.DomainStats{
		{UUID: vmUUID, Name: "web", State: libvirt.StateRunning, CPUTimeNs: ptr(f.cpu), VCPUs: ptr(2),
			MemAssignedKiB: ptr(uint64(1 << 20)), MemAvailableKiB: ptr(uint64(1 << 20)), MemUsableKiB: ptr(uint64(1 << 19))},
		{UUID: idleUUID, Name: "idle", State: libvirt.StateShutOff, VCPUs: ptr(1), MemAssignedKiB: ptr(uint64(1 << 20))},
	}, nil
}

func (f *statsSource) Describe(uuid string) (string, string, error) {
	if uuid == vmUUID {
		return "web", "Debian 12", nil
	}
	return "", "", errors.New("libvirt-secret-text")
}

type statsRig struct {
	t      *testing.T
	router http.Handler
	src    *statsSource
	clock  *stats.FakeClock
	dir    string
	s      *service.Stats
}

func newStatsRig(t *testing.T) *statsRig {
	t.Helper()
	logger.LogInitConsoleOnly()
	runtime := t.TempDir()
	previous := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	t.Cleanup(func() { config.CommonInfo.RuntimePath = previous })
	if err := os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &statsRig{t: t, src: &statsSource{}, dir: filepath.Join(t.TempDir(), "stats"),
		clock: stats.NewFakeClock(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))}
	s, err := service.NewStats(service.StatsOptions{Dir: r.dir, Clock: r.clock, Source: r.src})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r.s = s
	r.router = InitV2Router(nil, nil, s)
	return r
}

func (r *statsRig) ticks(n int) {
	for range n {
		r.s.Tick()
		r.clock.Advance(5 * time.Second)
	}
}

func (r *statsRig) do(method, path string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.send(method, path, "")
}

func (r *statsRig) send(method, path, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	request := httptest.NewRequest(method, "/v2/virt_management"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:40000"
	request.Header.Set("Authorization", "Internal s3cret")
	recorder := httptest.NewRecorder()
	r.router.ServeHTTP(recorder, request)
	return recorder
}

func (r *statsRig) get(path string, status int) map[string]json.RawMessage {
	r.t.Helper()
	recorder := r.do(http.MethodGet, path)
	if recorder.Code != status {
		r.t.Fatalf("GET %s: %d, want %d: %s", path, recorder.Code, status, recorder.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		r.t.Fatalf("GET %s: %v: %s", path, err, recorder.Body.String())
	}
	return body
}

// wantCode checks an error answer: its status and its body, the code alone.
func wantCode(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string, what string) {
	t.Helper()
	if recorder.Code != status || strings.TrimSpace(recorder.Body.String()) != `{"message":"`+code+`"}` {
		t.Errorf("%s: %d %s, want %d %s", what, recorder.Code, recorder.Body.String(), status, code)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sameKeys(t *testing.T, m map[string]json.RawMessage, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := keys(m); !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v", got, want)
	}
}

func TestStatsListRoute(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(2)
	body := r.get("/stats", http.StatusOK)
	sameKeys(t, body, "items", "notes")
	if string(body["notes"]) != "[]" {
		t.Errorf("notes %s", body["notes"])
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(body["items"], &items); err != nil || len(items) != 2 {
		t.Fatalf("items %s (%v)", body["items"], err)
	}
	for _, it := range items {
		sameKeys(t, it, "id", "name", "kind", "group", "state", "started_at", "detail", "cpu_percent", "memory_used", "memory_limit", "alerts")
	}
	web := items[1] // sorted by name: idle, web
	for k, want := range map[string]string{
		"id": `"vm:` + vmUUID + `"`, "name": `"web"`, "kind": `"vm"`, "group": `"Machines virtuelles"`, "state": `"running"`,
		"detail": `"Debian 12"`, "cpu_percent": "25", "memory_used": "536870912", "memory_limit": "1073741824", "alerts": "0",
	} {
		if string(web[k]) != want {
			t.Errorf("%s = %s, want %s", k, web[k], want)
		}
	}
	idle := items[0]
	if string(idle["state"]) != `"stopped"` || string(idle["cpu_percent"]) != "null" || string(idle["detail"]) != "null" {
		t.Errorf("idle %v", idle)
	}
}

func TestStatsSeriesRouteEncodedID(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(40) // 10:00:00 to 10:03:15
	body := r.get("/stats/"+vmID+"?range=15m", http.StatusOK)
	sameKeys(t, body, "id", "range", "step_seconds", "from", "to", "scale", "series", "notes")
	for k, want := range map[string]string{
		"id": `"vm:` + vmUUID + `"`, "range": `"15m"`, "step_seconds": "60",
		"scale": `{"cpu":"percent_of_one_core","cpu_allocation":2,"memory_limit":1073741824}`, "notes": "[]",
	} {
		if string(body[k]) != want {
			t.Errorf("%s = %s, want %s", k, body[k], want)
		}
	}
	var series []struct {
		Chart  string
		Points []json.RawMessage
	}
	if err := json.Unmarshal(body["series"], &series); err != nil || len(series) == 0 || len(series[0].Points) == 0 {
		t.Fatalf("series %s (%v)", body["series"], err)
	}
	var point []*float64
	if err := json.Unmarshal(series[0].Points[0], &point); err != nil || len(point) != 4 {
		t.Errorf("points[0] = %s (%v)", series[0].Points[0], err)
	}
	for _, sd := range series {
		if sd.Chart == "alert" {
			t.Error("an alert series is served")
		}
	}

	// the id arrives decoded whether or not its colon is encoded; echo and the
	// binder each unescape it, so a double encoding names the same VM
	for _, path := range []string{"/stats/vm:" + vmUUID, "/stats/vm%3a" + vmUUID, "/stats/vm%253A" + vmUUID} {
		if got := string(r.get(path, http.StatusOK)["id"]); got != `"vm:`+vmUUID+`"` {
			t.Errorf("%s: id %s", path, got)
		}
	}
}

func TestStatsSeriesBadRange(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)
	if got := string(r.get("/stats/"+vmID, http.StatusOK)["range"]); got != `"live"` {
		t.Errorf("default range %s", got)
	}
	for _, q := range []string{"?range=2d", "?range=", "?range=LIVE", "?range=%00%3Cjunk%3E", "?range=" + strings.Repeat("x", 10000)} {
		wantCode(t, r.do(http.MethodGet, "/stats/"+vmID+q), http.StatusBadRequest, "bad_range", q[:min(len(q), 20)])
	}
	// checked before the item is looked up
	wantCode(t, r.do(http.MethodGet, "/stats/"+nobody+"?range=2d"), http.StatusBadRequest, "bad_range", "unknown item")
	if code := r.do(http.MethodGet, "/stats/"+vmID+"?range=15m&range=1h").Code; code != http.StatusBadRequest {
		t.Errorf("repeated range: %d", code)
	}
}

func TestStatsSeriesUnknown(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)
	wantCode(t, r.do(http.MethodGet, "/stats/"+nobody), http.StatusNotFound, "unknown_item", "series")
	wantCode(t, r.do(http.MethodGet, "/stats/"+nobody+"/alerts"), http.StatusNotFound, "unknown_item", "alerts")
}

// Hostile ids never reach a 500 and never echo back.
func TestStatsHostileIDs(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)
	for _, path := range []string{
		"/stats/..", "/stats/%2e%2e", "/stats/vm:..%2F..%2Fetc%2Fpasswd-MARK", "/stats/" + vmID + "%2FMARK",
		"/stats/vm:a%00MARK", "/stats/vm:" + strings.Repeat("M", 10000) + "MARK", "/stats/%25MARK", "/stats/vm%25253AMARK",
	} {
		for _, suffix := range []string{"", "/alerts"} {
			recorder := r.do(http.MethodGet, path+suffix)
			if (recorder.Code != http.StatusNotFound && recorder.Code != http.StatusBadRequest) || strings.Contains(recorder.Body.String(), "MARK") {
				t.Errorf("%s%s: %d %s", path[:min(len(path), 60)], suffix, recorder.Code, recorder.Body.String())
			}
		}
	}
}

func TestStatsAlertsGetPut(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)
	body := r.get("/stats/"+vmID+"/alerts", http.StatusOK)
	sameKeys(t, body, "rules", "active")
	if string(body["rules"]) != defaultRules || string(body["active"]) != "[]" {
		t.Errorf("defaults %s %s", body["rules"], body["active"])
	}

	recorder := r.send(http.MethodPut, "/stats/"+vmID+"/alerts", `{"rules":[{"key":"cpu_high","enabled":true,"threshold":80,"minutes":10}]}`)
	want := `{"rules":[{"key":"cpu_high","enabled":true,"threshold":80,"minutes":10},{"key":"mem_high","enabled":true,"threshold":90,"minutes":5}],"active":[]}`
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != want {
		t.Errorf("PUT: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(r.do(http.MethodGet, "/stats/"+vmID+"/alerts").Body.String()); got != want {
		t.Errorf("GET after PUT: %s", got)
	}

	rule := func(key, enabled, threshold, minutes string) string {
		return `{"rules":[{"key":` + key + `,"enabled":` + enabled + `,"threshold":` + threshold + `,"minutes":` + minutes + `}]}`
	}
	for body, field := range map[string]string{
		rule(`"cpu_high"`, "true", "0", "3"):                        "rules[0].threshold",
		rule(`"cpu_high"`, "true", "101", "3"):                      "rules[0].threshold",
		rule(`"cpu_high"`, "true", `"80"`, "3"):                     "rules[0].threshold",
		rule(`"cpu_high"`, "true", "null", "3"):                     "rules[0].threshold",
		rule(`"cpu_high"`, "true", "80", "0"):                       "rules[0].minutes",
		rule(`"cpu_high"`, "true", "80", "121"):                     "rules[0].minutes",
		rule(`"cpu_high"`, "true", "80", "3.5"):                     "rules[0].minutes",
		rule(`"foo"`, "true", "80", "3"):                            "rules[0].key",
		rule(`"vm_crashed"`, "true", "80", "3"):                     "rules[0].key",
		rule(`"cpu_high"`, `"yes"`, "80", "3"):                      "rules[0].enabled",
		rule(`"cpu_high"`, "null", "80", "3"):                       "rules[0].enabled",
		`{"rules":[{"key":"cpu_high","threshold":80,"minutes":3}]}`: "rules[0].enabled",
		`{"rules":[{"key":"mem_high","enabled":true,"threshold":50,"minutes":5},{"key":"x","enabled":true,"threshold":50,"minutes":5}]}`: "rules[1].key",
		`{"rules":[[]]}`: "rules", `{"rules":null}`: "rules", `{}`: "rules", `[]`: "rules", `null`: "rules", ``: "rules",
		`{"rules":[]} x`: "rules", `{"rules":[`: "rules", `<MARK>`: "rules",
	} {
		wantCode(t, r.send(http.MethodPut, "/stats/"+vmID+"/alerts", body), http.StatusBadRequest, "bad_rules: "+field, body)
	}
	if got := strings.TrimSpace(r.do(http.MethodGet, "/stats/"+vmID+"/alerts").Body.String()); got != want {
		t.Errorf("a refusal changed the rules: %s", got)
	}

	ok := `{"rules":[{"key":"mem_high","enabled":true,"threshold":70,"minutes":2}]}`
	wantCode(t, r.send(http.MethodPut, "/stats/"+nobody+"/alerts", ok), http.StatusNotFound, "unknown_item", "unknown item")
	// listed, never ran: no history, so its rules would not be kept
	wantCode(t, r.send(http.MethodPut, "/stats/vm%3A"+idleUUID+"/alerts", ok), http.StatusNotFound, "unknown_item", "no history")
	if got := string(r.get("/stats/vm%3A"+idleUUID+"/alerts", http.StatusOK)["rules"]); got != defaultRules {
		t.Errorf("idle rules %s", got)
	}
}

func TestStatsPutBodyIsBounded(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)
	pad := func(n int) string {
		head, tail := `{"rules":[{"key":"cpu_high","enabled":true,"threshold":60,"minutes":3}],"pad":"`, `"}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	if code := r.send(http.MethodPut, "/stats/"+vmID+"/alerts", pad(64<<10)).Code; code != http.StatusOK {
		t.Errorf("64 KiB: %d", code)
	}
	wantCode(t, r.send(http.MethodPut, "/stats/"+vmID+"/alerts", pad(64<<10+1)), http.StatusBadRequest, "bad_rules: rules", "64 KiB + 1")
	wantCode(t, r.send(http.MethodPut, "/stats/"+vmID+"/alerts", `{"rules":[]}`+strings.Repeat(" ", 64<<10)), http.StatusBadRequest, "bad_rules: rules", "cut at the bound")
}

func TestStatsUnavailable(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)

	// applied but not written: not durable
	tmp := filepath.Join(r.dir, "alerts.json.tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	recorder := r.send(http.MethodPut, "/stats/"+vmID+"/alerts", `{"rules":[{"key":"mem_high","enabled":true,"threshold":70,"minutes":2}]}`)
	wantCode(t, recorder, http.StatusServiceUnavailable, "stats_unavailable", "failed save")
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}

	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.do(http.MethodGet, "/stats/"+vmID), http.StatusServiceUnavailable, "stats_unavailable", "closed store")

	// no service at all
	router := InitV2Router(nil, nil, nil)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/stats"}, {http.MethodGet, "/stats/" + vmID}, {http.MethodGet, "/stats/" + vmID + "/alerts"}, {http.MethodPut, "/stats/" + vmID + "/alerts"},
	} {
		request := httptest.NewRequest(rt.method, "/v2/virt_management"+rt.path, strings.NewReader(`{"rules":[]}`))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "127.0.0.1:40000"
		request.Header.Set("Authorization", "Internal s3cret")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		wantCode(t, recorder, http.StatusServiceUnavailable, "stats_unavailable", rt.method+" "+rt.path)
	}
}

func TestStatsNotCached(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)
	for _, path := range []string{"/stats", "/stats/" + vmID, "/stats/" + nobody, "/stats/" + vmID + "?range=2d", "/stats/" + vmID + "/alerts"} {
		if got := r.do(http.MethodGet, path).Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, got)
		}
	}
	for _, body := range []string{`{"rules":[]}`, `junk`} {
		if got := r.send(http.MethodPut, "/stats/"+vmID+"/alerts", body).Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("PUT %s: Cache-Control %q", body, got)
		}
	}
}

func TestStatsNoLibvirtTextInErrors(t *testing.T) {
	r := newStatsRig(t)
	r.src.err = errors.New("libvirt-secret-text")
	r.ticks(1)
	for _, path := range []string{"/stats", "/stats/" + vmID, "/stats/" + vmID + "/alerts"} {
		if body := r.do(http.MethodGet, path).Body.String(); strings.Contains(body, "libvirt-secret-text") {
			t.Errorf("%s: %s", path, body)
		}
	}
}

func TestStatsRoutesNeedToken(t *testing.T) {
	r := newStatsRig(t)
	r.ticks(1)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/vms"},
		{http.MethodGet, "/stats"},
		{http.MethodGet, "/stats/" + vmID},
		{http.MethodGet, "/stats/" + vmID + "/alerts"},
		{http.MethodPut, "/stats/" + vmID + "/alerts"},
	} {
		for _, auth := range []struct{ header, query string }{
			{}, {header: "Bearer junk"}, {query: "?token=junk"}, {header: "Internal s3cret"}, // the secret from afar is not internal
		} {
			request := httptest.NewRequest(rt.method, "/v2/virt_management"+rt.path+auth.query, strings.NewReader(`{"rules":[]}`))
			request.RemoteAddr = "203.0.113.5:40000"
			request.Header.Set("Content-Type", "application/json")
			if auth.header != "" {
				request.Header.Set("Authorization", auth.header)
			}
			recorder := httptest.NewRecorder()
			r.router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Errorf("%s %s %v: %d", rt.method, rt.path, auth, recorder.Code)
			}
		}
	}
}
