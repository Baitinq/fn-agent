package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Anthropic's prompt cache expires five minutes after its last read. While a tool call runs longer than that,
// replaying the previous request with a one-token output keeps the conversation prefix cached.
var cacheWarmInterval = 270 * time.Second

// A warm read costs 0.1x input and a cache rewrite 1.25x, so after about eleven warms re-caching would have been cheaper.
const cacheWarmMaxTicks = 10

type cacheWarmer struct {
	mu      sync.Mutex
	request []byte
	cancel  context.CancelFunc
}

func (a *Agent) rememberCacheWarmRequest(payload anthropicRequest) {
	payload.MaxTokens, payload.Stream = 1, false
	data, _ := json.Marshal(payload)
	a.cacheWarmer.mu.Lock()
	a.cacheWarmer.request = data
	a.cacheWarmer.mu.Unlock()
}

func (a *Agent) startCacheWarming() {
	a.cacheWarmer.mu.Lock()
	defer a.cacheWarmer.mu.Unlock()
	if a.cacheWarmer.request == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cacheWarmer.cancel = cancel
	go a.warmCache(ctx, a.cacheWarmer.request)
}

func (a *Agent) stopCacheWarming() {
	a.cacheWarmer.mu.Lock()
	defer a.cacheWarmer.mu.Unlock()
	if a.cacheWarmer.cancel != nil {
		a.cacheWarmer.cancel()
		a.cacheWarmer.cancel = nil
	}
	a.cacheWarmer.request = nil
}

func (a *Agent) warmCache(ctx context.Context, request []byte) {
	for range cacheWarmMaxTicks {
		select {
		case <-ctx.Done():
			return
		case <-time.After(cacheWarmInterval):
		}
		response, err := a.postAnthropic(ctx, request)
		if err != nil {
			return
		}
		var body struct {
			Usage anthropicUsage `json:"usage"`
		}
		err = json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			return
		}
		a.recordUsage(body.Usage.usage(), func(ToolEvent) {})
	}
}
