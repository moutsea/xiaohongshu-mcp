//go:build integration

package xiaohongshu

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	xhserrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

const exploreFixtureURL = "https://www.xiaohongshu.com/explore"

func isLoginFixtureURL(raw string) bool {
	return strings.HasPrefix(raw, exploreFixtureURL) || strings.HasPrefix(raw, "https://www.xiaohongshu.com/search_result")
}

const qrcodeFixtureHTML = `<!doctype html><html><body>` +
	`<div class="login-container"><img class="qrcode-img" src="data:image/png;base64,aaaa"></div>` +
	`</body></html>`

const loggedInFixtureHTML = `<!doctype html><html><body>` +
	`<div id="global" data-logged="1"><div id="app"></div><div class="main-container">` +
	`<li class="user side-bar-component"><div class="link-wrapper">` +
	`<a class="link-wrapper" href="/user/profile/fixture-user"><span class="channel">我</span></a>` +
	`</div></li></div></div></body></html>`

const missingQrcodeFixtureHTML = `<!doctype html><html><body><div class="main-container">no qrcode</div></body></html>`

const (
	loginQrSrc        = "data:image/png;base64,LOGINQR"
	downloadQrSrc     = "data:image/png;base64,DOWNLOAD"
	rednoteFixtureURL = "https://www.rednote.com/"
	captchaFixtureURL = "https://www.xiaohongshu.com/website-login/captcha"
	emptyFixtureHTML  = `<!doctype html><html><body></body></html>`
)

const loginButtonNoDialogHTML = `<!doctype html><html><body>` +
	`<a id="login-btn" href="/login" style="display:inline-block;width:120px;height:48px" onclick="event.preventDefault()">登录</a>` +
	`</body></html>`

const downloadQrOnlyHTML = `<!doctype html><html><body>` +
	`<img class="xhs-download-attribution-qr-image" alt="二维码" src="` + downloadQrSrc + `">` +
	`<div>在 rednote App 发现更多</div>` +
	`</body></html>`

const loginAndDownloadHTML = `<!doctype html><html><body>` +
	`<img class="xhs-download-attribution-qr-image" alt="二维码" src="` + downloadQrSrc + `">` +
	`<div class="login-container"><img class="qrcode-img" src="` + loginQrSrc + `"></div>` +
	`</body></html>`

const loginButtonOpensDialogHTML = `<!doctype html><html><body>` +
	`<img class="xhs-download-attribution-qr-image" alt="二维码" src="` + downloadQrSrc + `">` +
	`<div>在 rednote App 发现更多</div>` +
	`<a id="login-btn" href="/login" style="display:inline-block;width:120px;height:48px">登录</a>` +
	`<script>
		document.getElementById("login-btn").addEventListener("click", (event) => {
			event.preventDefault();
			const box = document.createElement("div");
			box.className = "login-container";
			box.innerHTML = '<img class="qrcode-img" src="` + loginQrSrc + `">';
			document.body.append(box);
		});
	</script>` +
	`</body></html>`

const accessLimitHTML = `<!doctype html><html><body>` +
	`<div class="access-wrapper"><img class="qrcode-img" src="data:image/png;base64,ACCESS"></div>` +
	`<div class="login-container"><img class="qrcode-img" src="` + loginQrSrc + `"></div>` +
	`<a id="login-btn" href="/login">登录</a>` +
	`</body></html>`

func serveNavigation(t *testing.T, page *rod.Page, handler func(*rod.Hijack)) {
	t.Helper()
	router := page.HijackRequests()
	router.MustAdd("*", func(h *rod.Hijack) {
		h.OnError = func(error) {}
		h.Response.SetHeader("Content-Type", "text/html; charset=utf-8")
		if h.Request.IsNavigation() {
			handler(h)
			return
		}
		h.Response.SetBody(emptyFixtureHTML)
	})
	go router.Run()
	t.Cleanup(func() { _ = router.Stop() })
}

// 拦截探索页文档，不访问小红书，也不加载登录态。
func serveExploreFixture(t *testing.T, page *rod.Page, document string, beforeRespond func()) {
	t.Helper()
	router := page.HijackRequests()
	router.MustAdd("*", func(h *rod.Hijack) {
		h.OnError = func(error) {}
		h.Response.SetHeader("Content-Type", "text/html; charset=utf-8")
		if h.Request.IsNavigation() && isLoginFixtureURL(h.Request.URL().String()) {
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

func TestFetchWaitsWhenLoginButtonCovered(t *testing.T) {
	b := newSearchFixtureBrowser(t)
	page := b.MustPage("about:blank")
	t.Cleanup(func() { _ = page.Close() })
	html := `<!doctype html><html><body>` +
		`<div class="login-container" style="position:fixed;inset:0;z-index:5"></div>` +
		`<a id="login-btn" href="/login" style="display:inline-block;width:120px;height:48px">登录</a>` +
		`<script>setTimeout(() => {` +
		`document.querySelector(".login-container").innerHTML = '<img class="qrcode-img" src="` + loginQrSrc + `">';` +
		`}, 4000);</script></body></html>`
	serveExploreFixture(t, page, html, nil)
	hook := &logCapture{}
	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })
	start := time.Now()
	img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
	require.NoError(t, err)
	require.False(t, loggedIn)
	require.Equal(t, loginQrSrc, img)
	require.Greater(t, time.Since(start), 3*time.Second)
	require.Less(t, time.Since(start), 12*time.Second)
	require.Contains(t, strings.Join(hook.lines, "\n"), "取码点击：登录框已在，等待二维码节点")
}

func TestFetchUsesSearchResult(t *testing.T) {
	b := newSearchFixtureBrowser(t)
	page := b.MustPage("about:blank")
	t.Cleanup(func() { _ = page.Close() })
	var search, explore int
	serveNavigation(t, page, func(h *rod.Hijack) {
		h.Response.SetHeader("Content-Type", "text/html; charset=utf-8")
		raw := h.Request.URL().String()
		switch {
		case strings.HasPrefix(raw, "https://www.xiaohongshu.com/search_result"):
			search++
			h.Response.SetBody(qrcodeFixtureHTML)
		case strings.HasPrefix(raw, exploreFixtureURL):
			explore++
			h.Response.SetBody(emptyFixtureHTML)
		default:
			h.Response.SetBody(emptyFixtureHTML)
		}
	})
	img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
	require.NoError(t, err)
	require.False(t, loggedIn)
	require.Equal(t, "data:image/png;base64,aaaa", img)
	require.Greater(t, search, 0)
	require.Zero(t, explore)
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
		require.ErrorContains(t, err, "登录入口未出现")
		require.False(t, loggedIn)
		require.Empty(t, img)
		require.GreaterOrEqual(t, elapsed, loginEntryTimeout)
		require.Less(t, elapsed, 20*time.Second)
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
		serveExploreFixture(t, page, loginButtonNoDialogHTML, nil)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			time.Sleep(4 * time.Second)
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

	t.Run("导航一直不完成时在时限内返回错误", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		release := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		serveExploreFixture(t, page, qrcodeFixtureHTML, func() { <-release })

		start := time.Now()
		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		elapsed := time.Since(start)
		require.Error(t, err)
		require.ErrorContains(t, err, "open explore failed")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.False(t, loggedIn)
		require.Empty(t, img)
		require.Less(t, elapsed, 25*time.Second)
		t.Logf("nav stall elapsed=%s err=%v", elapsed, err)
	})

	t.Run("已有登录二维码时不误取下载码", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, loginAndDownloadHTML, nil)

		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		require.NoError(t, err)
		require.False(t, loggedIn)
		require.Equal(t, loginQrSrc, img)
	})

	t.Run("只有下载码时返回错误", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, downloadQrOnlyHTML, nil)

		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		require.ErrorContains(t, err, "登录入口未出现")
		require.False(t, loggedIn)
		require.NotEqual(t, downloadQrSrc, img)
		require.Empty(t, img)
	})

	t.Run("未弹框时点击登录入口", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, loginButtonOpensDialogHTML, nil)

		start := time.Now()
		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		require.NoError(t, err)
		require.False(t, loggedIn)
		require.Equal(t, loginQrSrc, img)
		require.Less(t, time.Since(start), 15*time.Second)
	})

	t.Run("登录页跳转后仍走登录入口", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveNavigation(t, page, func(h *rod.Hijack) {
			switch {
			case isLoginFixtureURL(h.Request.URL().String()):
				h.Response.Payload().ResponseCode = http.StatusFound
				h.Response.SetHeader("Location", rednoteFixtureURL)
				h.Response.SetBody("")
			case strings.HasPrefix(h.Request.URL().String(), rednoteFixtureURL):
				h.Response.SetBody(loginButtonOpensDialogHTML)
			default:
				h.Response.SetBody(emptyFixtureHTML)
			}
		})

		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		require.NoError(t, err)
		require.False(t, loggedIn)
		require.Equal(t, loginQrSrc, img)
	})

	t.Run("访问限制页停止且不返回其中的码", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, accessLimitHTML, nil)

		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		require.ErrorIs(t, err, errPageAccessLimited)
		require.False(t, loggedIn)
		require.Empty(t, img)
	})

	t.Run("验证页停止且不返回其中的码", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveNavigation(t, page, func(h *rod.Hijack) {
			switch {
			case isLoginFixtureURL(h.Request.URL().String()):
				h.Response.Payload().ResponseCode = http.StatusFound
				h.Response.SetHeader("Location", captchaFixtureURL)
				h.Response.SetBody("")
			case strings.HasPrefix(h.Request.URL().String(), captchaFixtureURL):
				h.Response.SetBody(qrcodeFixtureHTML)
			default:
				h.Response.SetBody(emptyFixtureHTML)
			}
		})

		img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
		require.ErrorIs(t, err, xhserrors.ErrVerificationRequired)
		require.False(t, loggedIn)
		require.NotEqual(t, loginQrSrc, img)
		require.Empty(t, img)
	})
}
