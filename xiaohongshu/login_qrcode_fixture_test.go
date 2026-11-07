//go:build integration

package xiaohongshu

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

const exploreFixtureURL = "https://www.xiaohongshu.com/explore"

const qrcodeFixtureHTML = `<!doctype html><html><body>` +
	`<div class="login-container"><img class="qrcode-img" src="data:image/png;base64,aaaa"></div>` +
	`</body></html>`

const loggedInFixtureHTML = `<!doctype html><html><body>` +
	`<div class="main-container"><div class="user"><a class="link-wrapper"><span class="channel">me</span></a></div></div>` +
	`</body></html>`

const missingQrcodeFixtureHTML = `<!doctype html><html><body><div class="main-container">no qrcode</div></body></html>`

// 拦截探索页文档，不访问小红书，也不加载登录态。
func serveExploreFixture(t *testing.T, page *rod.Page, document string, beforeRespond func()) {
	t.Helper()
	router := page.HijackRequests()
	router.MustAdd("*", func(h *rod.Hijack) {
		h.OnError = func(error) {}
		h.Response.SetHeader("Content-Type", "text/html; charset=utf-8")
		if h.Request.IsNavigation() && strings.HasPrefix(h.Request.URL().String(), exploreFixtureURL) {
			if beforeRespond != nil {
				beforeRespond()
			}
			h.Response.SetBody(document)
			return
		}
		h.Response.SetBody(`<!doctype html><html><body></body></html>`)
	})
	go router.Run()
	t.Cleanup(func() { _ = router.Stop() })
}

func TestFetchQrcodeImageLocal(t *testing.T) {
	b := newSearchFixtureBrowser(t)

	t.Run("二维码节点缺失时返回错误", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, missingQrcodeFixtureHTML, nil)

		start := time.Now()
		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		elapsed := time.Since(start)

		require.Error(t, err)
		require.ErrorContains(t, err, "qrcode element not found")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.False(t, loggedIn)
		require.Empty(t, img)
		// 2s 停顿后再等 qrcodeWaitTimeout。必须在宿主 60s 内结束，且不是立刻失败。
		require.GreaterOrEqual(t, elapsed, qrcodeWaitTimeout)
		require.Less(t, elapsed, 35*time.Second)
		t.Logf("missing selector elapsed=%s err=%v", elapsed, err)
	})

	t.Run("节点存在时返回 src", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, qrcodeFixtureHTML, nil)

		start := time.Now()
		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.False(t, loggedIn)
		require.Equal(t, "data:image/png;base64,aaaa", img)
		require.Less(t, elapsed, 8*time.Second)
		t.Logf("present selector elapsed=%s", elapsed)
	})

	t.Run("已有登录标记时不取二维码", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, loggedInFixtureHTML, nil)

		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		require.NoError(t, err)
		require.True(t, loggedIn)
		require.Empty(t, img)
	})

	t.Run("空 src 仍返回错误", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, `<!doctype html><html><body><div class="login-container"><img class="qrcode-img" src=""></div></body></html>`, nil)

		_, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		require.ErrorContains(t, err, "qrcode src is empty")
		require.False(t, loggedIn)
	})

	t.Run("调用方取消后不必等满超时", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, missingQrcodeFixtureHTML, nil)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			time.Sleep(3 * time.Second)
			cancel()
		}()

		start := time.Now()
		_, _, err := NewLogin(page).FetchQrcodeImage(ctx)
		elapsed := time.Since(start)
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, elapsed, 10*time.Second)
		t.Logf("caller cancel elapsed=%s err=%v", elapsed, err)
	})

	t.Run("导航未完成时不受二维码超时约束", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })

		started := make(chan struct{})
		release := make(chan struct{})
		var startOnce sync.Once
		var releaseOnce sync.Once
		releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseFn)

		serveExploreFixture(t, page, qrcodeFixtureHTML, func() {
			startOnce.Do(func() { close(started) })
			<-release
		})

		type result struct {
			img      string
			loggedIn bool
			err      error
		}
		done := make(chan result, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					done <- result{err: errors.Errorf("panic: %v", r)}
				}
			}()
			img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
			done <- result{img: img, loggedIn: loggedIn, err: err}
		}()

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("导航没有进入本地拦截")
		}
		select {
		case res := <-done:
			t.Fatalf("导航仍被拦住时已返回: %+v", res)
		case <-time.After(6 * time.Second):
		}

		releaseFn()
		select {
		case res := <-done:
			require.NoError(t, res.err)
			require.False(t, res.loggedIn)
			require.Equal(t, "data:image/png;base64,aaaa", res.img)
		case <-time.After(15 * time.Second):
			t.Fatal("放开导航后没有返回")
		}
	})
}
