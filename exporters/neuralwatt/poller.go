package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// poller は /v1/quota を 1 本だけ、設定した間隔で呼ぶ。/metrics の scrape では呼ばない。
type poller struct {
	client   *quotaClient
	st       *state
	clock    clock
	interval time.Duration
	log      *slog.Logger
	// jitter は [0,1) の乱数（テストで差し替える）。
	jitter func() float64

	// 以下は run のゴルーチンだけが触る。
	consecutive429 int
	logged         bool
	lastResult     string
}

func newPoller(client *quotaClient, st *state, clk clock, interval time.Duration, log *slog.Logger) *poller {
	return &poller{client: client, st: st, clock: clk, interval: interval, log: log, jitter: rand.Float64}
}

// step は 1 回取得して状態に反映し、次の取得までの待ち時間を返す。
func (p *poller) step(ctx context.Context) time.Duration {
	start := p.clock.Now()
	snap, ferr := p.client.fetch(ctx)
	end := p.clock.Now()
	if ctx.Err() != nil {
		// 終了中の中断は結果として数えない。
		return p.interval
	}
	result := resultOK
	if ferr != nil {
		result = ferr.result
	}
	p.st.record(result, end.Sub(start), end, snap)
	p.logTransition(result, ferr)

	if result != resultRateLimited {
		p.consecutive429 = 0
		return p.interval
	}
	p.consecutive429++
	if ferr.retryAfter != nil {
		// Retry-After の間は再取得しない。間隔より短い指定でも間隔は守る。
		return max(*ferr.retryAfter, p.interval)
	}
	return p.backoff()
}

// backoff は Retry-After がない 429 の待ち時間。指数で増やし、上限は 10 分。ジッターで下半分に散らす（間隔は下回らない）。
func (p *poller) backoff() time.Duration {
	b := maxBackoff
	if shift := p.consecutive429; shift < 20 {
		if v := p.interval << shift; v > 0 && v < maxBackoff {
			b = v
		}
	}
	d := b/2 + time.Duration(p.jitter()*float64(b/2))
	return max(d, p.interval)
}

// logTransition は成功↔失敗と結果の分類が変わったときだけ出す。ログにはキー・応答本文・下位のエラー文を含めない。
func (p *poller) logTransition(result string, ferr *fetchError) {
	if p.logged && p.lastResult == result {
		return
	}
	p.logged = true
	p.lastResult = result
	if ferr == nil {
		p.log.Info("fetch succeeded", "result", result)
		return
	}
	p.log.Warn("fetch failed", "result", result, "reason", ferr.msg)
}

func (p *poller) run(ctx context.Context) {
	for {
		delay := p.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.clock.After(delay):
		}
	}
}
