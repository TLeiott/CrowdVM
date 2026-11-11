package node

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func uiPost(t *testing.T, c *http.Client, url, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set(UIHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getStatus(t *testing.T, c *http.Client, url string) Status {
	t.Helper()
	resp, err := c.Get(url + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st Status
	json.NewDecoder(resp.Body).Decode(&st)
	return st
}

func TestUILoginCookie(t *testing.T) {
	h := start(t, nil)
	srv := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	if st := getStatus(t, c, srv.URL); st.Authorized || st.Settings != nil {
		t.Fatalf("anonymous status is authorized: %+v", st.Authorized)
	}
	// The token itself is no login link; a single-use code from the CLI is.
	resp, _ := c.Get(srv.URL + "/login?c=tok")
	if resp.StatusCode != 403 {
		t.Fatalf("token as login code: %d", resp.StatusCode)
	}
	if resp := uiPost(t, c, srv.URL+"/api/login-code", "", ""); resp.StatusCode != 401 {
		t.Fatalf("login code without token: %d", resp.StatusCode)
	}
	resp = uiPost(t, http.DefaultClient, srv.URL+"/api/login-code", "tok", "")
	var lc struct{ Code string }
	json.NewDecoder(resp.Body).Decode(&lc)
	if lc.Code == "" || strings.Contains(lc.Code, "tok") {
		t.Fatalf("login code %q", lc.Code)
	}
	resp, _ = c.Get(srv.URL + "/login?c=" + lc.Code)
	if resp.StatusCode != 200 || resp.Request.URL.RawQuery != "" {
		t.Fatalf("login did not redirect to the page: %d %s", resp.StatusCode, resp.Request.URL)
	}
	if rp := resp.Header.Get("Referrer-Policy"); rp != "no-referrer" {
		t.Fatalf("referrer policy %q", rp)
	}
	u, _ := url.Parse(srv.URL)
	for _, ck := range jar.Cookies(u) {
		if ck.Value == "tok" {
			t.Fatal("the cookie holds the token")
		}
	}
	if resp, _ := http.Get(srv.URL + "/login?c=" + lc.Code); resp.StatusCode != 403 {
		t.Fatalf("login code reused: %d", resp.StatusCode)
	}
	st := getStatus(t, c, srv.URL)
	if !st.Authorized || st.Settings["cpus"] != "8" {
		t.Fatalf("logged-in status: %+v %v", st.Authorized, st.Settings)
	}
	// The cookie authorizes the page's own writes (with the header) ...
	if resp := uiPost(t, c, srv.URL+"/api/pause", "", `{"paused":true}`); resp.StatusCode != 200 {
		t.Fatalf("pause with cookie: %d", resp.StatusCode)
	}
	if resp := uiPost(t, c, srv.URL+"/api/config", "", `{"key":"schedule","value":"weekends"}`); resp.StatusCode != 200 {
		t.Fatalf("schedule with cookie: %d", resp.StatusCode)
	}
	// ... but not settings that weaken isolation, nor CLI-only calls.
	if resp := uiPost(t, c, srv.URL+"/api/config", "", `{"key":"driver","value":"process"}`); resp.StatusCode != 403 {
		t.Fatalf("driver with cookie: %d", resp.StatusCode)
	}
	if resp := uiPost(t, c, srv.URL+"/api/images/x/import", "", `{"path":"/etc/passwd"}`); resp.StatusCode != 401 {
		t.Fatalf("image import with cookie: %d", resp.StatusCode)
	}
	if resp := uiPost(t, c, srv.URL+"/api/login-code", "", ""); resp.StatusCode != 401 {
		t.Fatalf("login code with cookie: %d", resp.StatusCode)
	}
	if resp := uiPost(t, http.DefaultClient, srv.URL+"/api/config", "tok", `{"key":"net.allow-lan","value":"true"}`); resp.StatusCode != 200 {
		t.Fatalf("restart-only key with the token: %d", resp.StatusCode)
	}
	// A handler without a token refuses every write.
	srv2 := httptest.NewServer(h.node.UIHandler(""))
	defer srv2.Close()
	if resp := uiPost(t, http.DefaultClient, srv2.URL+"/api/pause", "", `{"paused":false}`); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func TestUIConfigLiveAndRestart(t *testing.T) {
	h := start(t, nil)
	srv := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv.Close()

	// A key edited in the file by the CLI while the daemon runs survives.
	disk, _ := LoadConfig(h.cfg.DataDir)
	disk.WarmPool = 2
	disk.Save()

	resp := uiPost(t, http.DefaultClient, srv.URL+"/api/config", "tok", `{"key":"cpus","value":"4"}`)
	var out struct{ Applied, RestartNeeded bool }
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || !out.Applied || out.RestartNeeded {
		t.Fatalf("cpus: %d %+v", resp.StatusCode, out)
	}
	if got := h.node.Status().Capacity.VCPU; got != 4 {
		t.Fatalf("capacity not applied live: %d", got)
	}
	waitFor(t, "new capacity sent", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		for _, b := range h.srv.beats {
			if b.Info != nil && b.Info.Capacity.VCPU == 4 {
				return true
			}
		}
		return false
	})
	disk, _ = LoadConfig(h.cfg.DataDir)
	if disk.CPUs != 4 || disk.WarmPool != 2 {
		t.Fatalf("file: cpus %d warm %d", disk.CPUs, disk.WarmPool)
	}

	// Restart-only keys are saved and listed.
	resp = uiPost(t, http.DefaultClient, srv.URL+"/api/config", "tok", `{"key":"net.allow-lan","value":"true"}`)
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Applied || !out.RestartNeeded {
		t.Fatalf("net.allow-lan: %+v", out)
	}
	if p := h.node.Status().RestartPending; len(p) != 1 || p[0] != "net.allow-lan" {
		t.Fatalf("pending %v", p)
	}
	if h.node.config().Net.AllowLAN {
		t.Fatal("restart-only key applied live")
	}
	// Set back to the running value: nothing is pending any more.
	uiPost(t, http.DefaultClient, srv.URL+"/api/config", "tok", `{"key":"net.allow-lan","value":"false"}`)
	if p := h.node.Status().RestartPending; len(p) != 0 {
		t.Fatalf("pending after reverting %v", p)
	}
	// Invalid values are refused and not saved.
	if resp := uiPost(t, http.DefaultClient, srv.URL+"/api/config", "tok", `{"key":"cpus","value":"-3"}`); resp.StatusCode != 400 {
		t.Fatalf("bad cpus: %d", resp.StatusCode)
	}

	// Restart: refused without a service manager, delegated with one.
	if resp := uiPost(t, http.DefaultClient, srv.URL+"/api/restart", "tok", ""); resp.StatusCode != 409 {
		t.Fatalf("restart unsupervised: %d", resp.StatusCode)
	}
	done := make(chan struct{})
	h.node.SetRestarter(func() { close(done) })
	if !h.node.Status().CanRestart {
		t.Fatal("canRestart false")
	}
	if resp := uiPost(t, http.DefaultClient, srv.URL+"/api/restart", "tok", ""); resp.StatusCode != 202 {
		t.Fatalf("restart: %d", resp.StatusCode)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("restarter not called")
	}
}

func TestUIShowsEarningsAndEvacuation(t *testing.T) {
	h := start(t, nil)
	dl := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	h.srv.mu.Lock()
	h.srv.hbResp = proto.HeartbeatResponse{Drained: true, EarnedCredits: 41.5, Earned24h: 3.25,
		Evacuation: &proto.Evacuation{StartedAt: time.Now().UTC().Truncate(time.Second), Deadline: dl}}
	h.srv.mu.Unlock()
	waitFor(t, "earnings in status", func() bool { return h.node.Status().EarnedCredits == 41.5 })
	st := h.node.Status()
	if st.Earned24h != 3.25 || !st.Drained || st.Evacuation == nil || !st.Evacuation.Deadline.Equal(dl) {
		t.Fatalf("status %+v %+v", st.Earned24h, st.Evacuation)
	}
}

func TestScheduleParse(t *testing.T) {
	for _, bad := range []string{"funday", "mon 25:00-26:00", "mon 10:00-10:00", "mon 9-17", "mon 10:00-12:00 extra"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	s, err := ParseSchedule("mon-fri 19:00-08:00; sat,sun")
	if err != nil {
		t.Fatal(err)
	}
	at := func(day, hhmm string) time.Time {
		tm, _ := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, time.Local)
		return tm
	}
	// 2026-11-09 is a Monday.
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at("2026-11-09", "07:59"), false}, // Mon morning: Sunday's window has no carry-over
		{at("2026-11-09", "12:00"), false},
		{at("2026-11-09", "19:00"), true},
		{at("2026-11-10", "07:59"), true}, // Tue morning: Monday night carries over
		{at("2026-11-10", "08:00"), false},
		{at("2026-11-14", "12:00"), true}, // Saturday all day
		{at("2026-11-15", "23:59"), true},
		{at("2026-11-16", "07:00"), false}, // Mon: Sunday is all-day only
		{at("2026-11-15", "00:00"), true},
		{at("2026-11-14", "07:00"), true}, // Sat 07:00: weekend and Friday's carry-over
	}
	for _, c := range cases {
		if got := s.Active(c.t); got != c.want {
			t.Errorf("%s: active %v, want %v", c.t.Format("Mon 15:04"), got, c.want)
		}
	}
	if next := s.NextChange(at("2026-11-09", "12:00")); !next.Equal(at("2026-11-09", "19:00")) {
		t.Errorf("next change %v", next)
	}
	if next := s.NextChange(at("2026-11-13", "20:00")); !next.Equal(at("2026-11-16", "00:00")) {
		t.Errorf("weekend next change %v", next)
	}
	if s, _ := ParseSchedule("daily"); !s.NextChange(time.Now()).IsZero() {
		t.Error("an always-on schedule changes")
	}
	if s, _ := ParseSchedule("weekdays 09:00-17:00"); !s.Active(at("2026-11-13", "16:59")) || s.Active(at("2026-11-14", "10:00")) {
		t.Error("weekdays")
	}
}

func TestEffectivePause(t *testing.T) {
	c := Config{DataDir: t.TempDir(), Schedule: "daily 09:00-17:00"}
	noon := time.Date(2026, 11, 11, 12, 0, 0, 0, time.Local)
	night := time.Date(2026, 11, 11, 20, 0, 0, 0, time.Local)
	if ps := c.EffectivePause(noon); ps.Paused || ps.Until.Hour() != 17 {
		t.Fatalf("noon %+v", ps)
	}
	if ps := c.EffectivePause(night); !ps.Paused || ps.Reason != PauseSchedule || ps.Until.Hour() != 9 {
		t.Fatalf("night %+v", ps)
	}
	// A hold wins until it expires; the master switch wins over everything.
	c.ScheduleHold = &ScheduleHold{Offer: true, Until: night.Add(time.Hour)}
	if ps := c.EffectivePause(night); ps.Paused {
		t.Fatalf("hold offer %+v", ps)
	}
	// A hold that offers until the window opens offers until it closes.
	c.ScheduleHold = &ScheduleHold{Offer: true, Until: time.Date(2026, 11, 12, 9, 0, 0, 0, time.Local)}
	if ps := c.EffectivePause(night); ps.Paused || !ps.Until.Equal(time.Date(2026, 11, 12, 17, 0, 0, 0, time.Local)) {
		t.Fatalf("hold until the window: %+v", ps)
	}
	c.ScheduleHold = &ScheduleHold{Offer: true, Until: night.Add(time.Hour)}
	if ps := c.EffectivePause(night.Add(2 * time.Hour)); !ps.Paused || ps.Reason != PauseSchedule {
		t.Fatalf("expired hold %+v", ps)
	}
	c.Paused = true
	if ps := c.EffectivePause(noon); !ps.Paused || ps.Reason != PauseOwner {
		t.Fatalf("master switch %+v", ps)
	}
	// Setting the hold through the config key ends it at the next boundary.
	c.Paused = false
	if err := c.Set("schedule-hold", "offer"); err != nil {
		t.Fatal(err)
	}
	if h := c.ScheduleHold; h == nil || !h.Offer || h.Until.Before(time.Now()) {
		t.Fatalf("hold %+v", h)
	}
	if err := c.Set("schedule", "weekends"); err != nil || c.ScheduleHold != nil {
		t.Fatalf("a new schedule keeps the old hold: %v %+v", err, c.ScheduleHold)
	}
	if err := (&Config{}).Set("schedule-hold", "offer"); err == nil {
		t.Fatal("hold without a schedule")
	}
}

func TestScheduleSentToServer(t *testing.T) {
	// Outside every window: the node advertises a scheduled pause.
	h := start(t, func(c *Config) { c.Schedule = "mon 00:00-00:01" })
	if time.Now().Weekday() == time.Monday && time.Now().Hour() == 0 && time.Now().Minute() == 0 {
		t.Skip("inside the one-minute window")
	}
	waitFor(t, "scheduled pause sent", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		for _, b := range h.srv.beats {
			if b.Info != nil && b.Info.Paused && b.Info.PauseReason == PauseSchedule {
				return true
			}
		}
		return false
	})
	// Offering through a hold is sent without a restart.
	h.srv.mu.Lock()
	n0 := len(h.srv.beats)
	h.srv.mu.Unlock()
	if _, err := h.node.SetConfig("schedule-hold", "offer"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hold sent", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		for _, b := range h.srv.beats[n0:] {
			if b.Info != nil && !b.Info.Paused {
				return true
			}
		}
		return false
	})
}

func TestUITokenFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadUIToken(dir); err == nil {
		t.Fatal("token before creation")
	}
	a, err := UIToken(dir)
	if err != nil || len(a) != 64 {
		t.Fatalf("token %q %v", a, err)
	}
	b, _ := UIToken(dir)
	if a != b {
		t.Fatal("token changed between starts")
	}
	if fi, _ := os.Stat(dir + "/" + uiTokenFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	// An empty file (a crash mid-write before this fix) is replaced.
	os.WriteFile(dir+"/"+uiTokenFile, nil, 0o600)
	if c, err := UIToken(dir); err != nil || len(c) != 64 || c == a {
		t.Fatalf("after empty file: %q %v", c, err)
	}
}

func TestCreateRefusedOutsideSchedule(t *testing.T) {
	h := start(t, func(c *Config) { c.Schedule = "mon 00:00-00:01" })
	if h.cfg.EffectivePause(time.Now()).Paused == false {
		t.Skip("inside the one-minute window")
	}
	_, err := h.node.CreateVM(proto.VMSpec{SandboxID: "bx_sched", Size: proto.Size{Name: "small", VCPU: 1, MemoryMiB: 512, DiskGiB: 1}})
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("create outside the schedule: %v", err)
	}
}
