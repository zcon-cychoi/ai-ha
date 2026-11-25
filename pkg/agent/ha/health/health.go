// Package health measures what an HA node's assignment asks for: its service
// unit, port and health API, the standby's view of the primary, its GPUs and
// the replication state of its volume (TM-AI-HA-02 tables 5-1 and 5-2).
//
// It measures and does not judge. What a value should be depends on the node's
// role, which is the framework's record; a stopped service is right on the
// standby and an outage on the primary. Every check runs whatever the role, so
// that a node whose role changes does not change what it does.
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloud-barista/ai-ha/pkg/agent/ha/drbd"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

const (
	// dialTimeout bounds opening a TCP connection. A port on the node itself
	// or across a VPC answers in milliseconds; one that takes seconds is as
	// good as closed for a service clients are waiting on.
	dialTimeout = 2 * time.Second
	// commandTimeout bounds systemctl and nvidia-smi. A driver that hangs
	// nvidia-smi is itself a finding, and must not hold the report back.
	commandTimeout = 10 * time.Second
	// maxBodyBytes is how much of a health API's answer is read before the
	// connection is closed. Only the status is used.
	maxBodyBytes = 64 << 10
	// localHost is where the service is reached on the node itself.
	localHost = "127.0.0.1"
)

// Limits the framework enforces on a report. A value past them would have the
// whole report refused, so they are clipped here instead.
const (
	maxErrorLen = 1024
	maxURLLen   = 2400
	maxHostLen  = 255
	maxStateLen = 32
)

// Collector takes a node's measurements.
type Collector struct {
	runner sysinfo.Runner
	drbd   *drbd.Reader
	http   *http.Client
	dial   func(ctx context.Context, network, address string) (net.Conn, error)
	now    func() time.Time
}

// NewCollector returns a Collector that runs commands through runner, or
// through sysinfo.ExecRunner when runner is nil.
func NewCollector(runner sysinfo.Runner) *Collector {
	if runner == nil {
		runner = sysinfo.ExecRunner{Timeout: commandTimeout}
	}
	dialer := &net.Dialer{Timeout: dialTimeout}
	return &Collector{
		runner: runner,
		drbd:   drbd.NewReader(runner),
		http: &http.Client{
			Timeout: model.HAHealthAPITimeout,
			Transport: &http.Transport{
				// Never through a proxy: the target is the node itself or its
				// peer on the private network, and a proxy configured for the
				// host's outbound traffic would measure the proxy instead.
				Proxy:             nil,
				DialContext:       dialer.DialContext,
				DisableKeepAlives: true,
			},
			// A redirect is an answer, and not a healthy one: following it
			// would report the health of wherever it points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		dial: dialer.DialContext,
		now:  time.Now,
	}
}

// Measure takes the measurements a assigns. With no assignment there is
// nothing to measure, and the report only asks the framework for one.
//
// The checks run concurrently. Each is bounded on its own, so the report is
// as late as the slowest check rather than the sum of them, and one wedged
// check costs only its own result.
func (c *Collector) Measure(ctx context.Context, a *model.HAAssignment) model.AgentHealthReq {
	req := model.AgentHealthReq{CollectedAt: c.now().UTC(), Hostname: hostname()}
	if a == nil {
		return req
	}
	req.GroupID, req.Role = a.GroupID, a.Role

	svc := a.Service
	var wg sync.WaitGroup
	// Each check writes only its own field.
	wg.Go(func() { req.Service = c.unit(ctx, svc.Unit) })
	wg.Go(func() { req.Port = c.port(ctx, localHost, svc.Port) })
	if svc.HealthPath != "" {
		wg.Go(func() { req.HealthAPI = c.call(ctx, localHost, svc.Port, svc.HealthPath) })
	}
	if p := a.Probe; p != nil {
		wg.Go(func() { req.PeerPort = c.port(ctx, p.Address, p.Port) })
		if p.HealthPath != "" {
			wg.Go(func() { req.PeerHealthAPI = c.call(ctx, p.Address, p.Port, p.HealthPath) })
		}
	}
	if svc.ExpectedGPUs > 0 {
		wg.Go(func() { req.GPU = c.gpus(ctx) })
	}
	// The framework names a resource for every group it creates. A report
	// without replication status is judged as unreported, which is the truth
	// for an assignment that names none.
	if res := a.Resource.Name; res != "" {
		wg.Go(func() { req.Replication = c.replication(ctx, res) })
	}
	wg.Wait()
	return req
}

// replication reads the state of the replication resource.
func (c *Collector) replication(ctx context.Context, resource string) *model.HAReplicationStatus {
	st, err := c.drbd.Status(ctx, resource)
	if err != nil {
		return &model.HAReplicationStatus{Error: clip(describe(err, "drbdsetup"), maxErrorLen)}
	}
	return &model.HAReplicationStatus{
		Role:           clip(st.Role, maxStateLen),
		PeerRole:       clip(st.PeerRole, maxStateLen),
		Connection:     clip(st.Connection, maxStateLen),
		Sync:           clip(st.Replication, maxStateLen),
		Disk:           clip(st.Disk, maxStateLen),
		PeerDisk:       clip(st.PeerDisk, maxStateLen),
		OutOfSyncBytes: st.OutOfSyncBytes,
	}
}

// unit reads a systemd unit's state. "systemctl show" is used rather than
// "is-active" because it exits zero whatever the state, so the state is read
// from its output rather than inferred from an exit status.
func (c *Collector) unit(ctx context.Context, unit string) *model.HAUnitCheck {
	out, err := c.runner.Run(ctx, "systemctl", "show", "--property=LoadState,ActiveState,SubState", "--", unit)
	if err != nil {
		return &model.HAUnitCheck{Error: clip(describe(err, "systemctl"), maxErrorLen)}
	}
	props := parseProperties(string(out))
	check := &model.HAUnitCheck{
		LoadState:   clip(props["LoadState"], maxStateLen),
		ActiveState: clip(props["ActiveState"], maxStateLen),
		SubState:    clip(props["SubState"], maxStateLen),
	}
	if check.ActiveState == "" {
		check.Error = "systemctl reported no ActiveState"
	}
	return check
}

// port opens and closes a TCP connection.
func (c *Collector) port(ctx context.Context, host string, port int) *model.HAPortCheck {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	check := &model.HAPortCheck{Address: clip(address, 300)}
	conn, err := c.dial(ctx, "tcp", address)
	if err != nil {
		check.Error = clip(err.Error(), maxErrorLen)
		return check
	}
	_ = conn.Close()
	check.Open = true
	return check
}

// call requests a health API and times the answer.
func (c *Collector) call(ctx context.Context, host string, port int, path string) *model.HAHTTPCheck {
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + path
	check := &model.HAHTTPCheck{URL: clip(url, maxURLLen)}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		check.Error = clip(err.Error(), maxErrorLen)
		return check
	}
	start := c.now()
	resp, err := c.http.Do(req)
	check.LatencyMs = c.now().Sub(start).Milliseconds()
	if err != nil {
		check.Error = clip(err.Error(), maxErrorLen)
		return check
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	_ = resp.Body.Close()
	check.StatusCode = resp.StatusCode
	return check
}

// gpus counts the GPUs the driver reports.
func (c *Collector) gpus(ctx context.Context) *model.HAGPUCheck {
	out, err := c.runner.Run(ctx, "nvidia-smi", "--query-gpu=index", "--format=csv,noheader")
	if err != nil {
		return &model.HAGPUCheck{Error: clip(describe(err, "nvidia-smi"), maxErrorLen)}
	}
	n := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return &model.HAGPUCheck{Count: n}
}

// parseProperties reads "Key=Value" lines.
func parseProperties(out string) map[string]string {
	props := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return props
}

// describe renders a command failure, naming a missing tool plainly.
func describe(err error, tool string) string {
	if errors.Is(err, sysinfo.ErrCommandNotFound) {
		return fmt.Sprintf("%s is not installed", tool)
	}
	return err.Error()
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return clip(h, maxHostLen)
}

// clip shortens s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// s[n] is the first byte left out; cutting before a character's start
	// keeps the character whole.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
