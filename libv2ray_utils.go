package libv2ray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corenet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
	corestats "github.com/xtls/xray-core/features/stats"
	coreserial "github.com/xtls/xray-core/infra/conf/serial"
)

// QueryAllOutboundTrafficStats retrieves and resets all outbound traffic counters.
// Returns a single-line text in format: tag,direction,value;tag,direction,value;
// Returns an empty string if the stats manager is not initialized or no counters exist.
func (x *CoreController) QueryAllOutboundTrafficStats() string {
	if x.statsManager == nil {
		return ""
	}

	var b strings.Builder

	x.statsManager.VisitCounters(func(name string, counter corestats.Counter) bool {
		parts := strings.Split(name, ">>>")
		if len(parts) != 4 || parts[0] != "outbound" || parts[2] != "traffic" {
			return true
		}

		tag := parts[1]
		direct := parts[3]
		value := counter.Set(0)
		if value <= 0 {
			return true
		}

		b.WriteString(tag)
		b.WriteByte(',')
		b.WriteString(direct)
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(value, 10))
		b.WriteByte(';')
		return true
	})
	return b.String()
}

// MeasureDelay measures network latency to a specified URL through the current core instance
// Uses a 12-second timeout context and returns the round-trip time in milliseconds
// An error is returned if the connection fails or returns an unexpected status
func (x *CoreController) MeasureDelay(url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	return measureInstDelay(ctx, x.coreInstance, url)
}

// OutboundDelayResult holds the delay measurement and egress IP info.
type OutboundDelayResult struct {
	Delay   int64  `json:"delay"`
	Country string `json:"country"`
	IP      string `json:"ip"`
}

// MeasureOutboundDelay measures the outbound delay for a given configuration and URL
func MeasureOutboundDelay(ConfigureFileContent string, url string) (int64, error) {
	result, err := MeasureOutboundDelayWithInfo(ConfigureFileContent, url, "")
	if err != nil {
		return -1, err
	}
	return result.Delay, nil
}

// MeasureOutboundDelayWithInfo measures outbound delay AND fetches egress IP info
// through the same core.Dial path. Returns JSON with delay, country, and IP.
func MeasureOutboundDelayWithInfo(ConfigureFileContent string, url string, ipApiUrl string) (*OutboundDelayResult, error) {
	config, err := coreserial.LoadJSONConfig(strings.NewReader(ConfigureFileContent))
	if err != nil {
		return nil, fmt.Errorf("config load error: %w", err)
	}

	config.Inbound = nil
	var essentialApp []*serial.TypedMessage
	for _, app := range config.App {
		if app.Type == "xray.app.proxyman.OutboundConfig" ||
			app.Type == "xray.app.dispatcher.Config" ||
			app.Type == "xray.app.log.Config" {
			essentialApp = append(essentialApp, app)
		}
	}
	config.App = essentialApp

	inst, err := core.New(config)
	if err != nil {
		return nil, fmt.Errorf("instance creation failed: %w", err)
	}

	if err := inst.Start(); err != nil {
		return nil, fmt.Errorf("startup failed: %w", err)
	}
	defer inst.Close()

	// Measure delay
	delay, _ := measureInstDelay(context.Background(), inst, url)

	// Fetch egress IP info through the same dial path
	country, ip := fetchEgressInfo(context.Background(), inst, ipApiUrl)

	return &OutboundDelayResult{
		Delay:   delay,
		Country: country,
		IP:      ip,
	}, nil
}

// fetchEgressInfo fetches egress IP info through the core.Dial path.
func fetchEgressInfo(ctx context.Context, inst *core.Instance, ipApiUrl string) (string, string) {
	if ipApiUrl == "" {
		ipApiUrl = "https://ipinfo.io/json"
	}

	tr := &http.Transport{
		TLSHandshakeTimeout: 6 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, err := corenet.ParseDestination(fmt.Sprintf("%s:%s", network, addr))
			if err != nil {
				return nil, err
			}
			return core.Dial(ctx, inst, dest)
		},
	}
	defer tr.CloseIdleConnections()

	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, "GET", ipApiUrl, nil)
	if err != nil {
		return "", ""
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", ""
	}

	var ipInfo struct {
		Country string `json:"country"`
		IP      string `json:"ip"`
	}
	if err := json.Unmarshal(body, &ipInfo); err != nil {
		return "", ""
	}

	return ipInfo.Country, ipInfo.IP
}

// measureInstDelay measures the delay for an instance to a given URL
func measureInstDelay(ctx context.Context, inst *core.Instance, url string) (int64, error) {
	if inst == nil {
		return -1, errors.New("core instance is nil")
	}

	if url == "" {
		url = "https://www.google.com/generate_204"
	}

	tr := &http.Transport{
		TLSHandshakeTimeout: 6 * time.Second,
		DisableKeepAlives:   false,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, err := corenet.ParseDestination(fmt.Sprintf("%s:%s", network, addr))
			if err != nil {
				return nil, err
			}
			return core.Dial(ctx, inst, dest)
		},
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   12 * time.Second,
	}

	defer tr.CloseIdleConnections()

	// Warm, unmeasured request. The delay previously reported was the whole
	// cold HTTPS request — DNS + TCP + tunnel handshake + TLS + the GET
	// itself, about four RTTs, so a ~100 ms link displayed ~500 ms. The
	// warmup completes DNS, the tunnel handshake and TLS, and keep-alives
	// retain the connection, so the request that follows rides an
	// established path and its round trip is what gets timed — one RTT,
	// which is what a "ping" promises.
	if warm, err := http.NewRequestWithContext(ctx, "GET", url, nil); err == nil {
		if resp, err := client.Do(warm); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	var minDuration int64 = -1
	success := false
	var lastErr error

	// Two attempts, min kept. The first attempt pays the cold path — wireguard
	// handshake, TCP, TLS — roughly 4-5× the server's real RTT. The second rides
	// the warm keep-alive connection and lands ≈ one real RTT. A single attempt
	// would therefore always report the cold number (a plain-100ms server reads
	// ~500ms), which is how the app showed 5× the real delay until this was
	// restored to the 1.7.x behavior.
	const attempts = 2
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			if !success {
				return -1, ctx.Err()
			}
			return minDuration, nil
		default:
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create HTTP request: %w", err)
			continue
		}

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			lastErr = fmt.Errorf("invalid status: %s", resp.Status)
			continue
		}

		if err != nil {
			lastErr = fmt.Errorf("failed to read response body: %w", err)
			continue
		}

		duration := time.Since(start).Milliseconds()
		if !success || duration < minDuration {
			minDuration = duration
		}

		success = true
	}
	if !success {
		return -1, lastErr
	}
	return minDuration, nil
}
