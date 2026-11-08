//go:build integration

package xiaohongshu

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	xhserrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

const guestFixtureHTML = `<!doctype html><html><body><div id="global" data-logged="0">` +
	`<div id="app"></div><div class="main-container">` +
	`<a id="login-btn" href="/login">登录</a>` +
	`<li class="user side-bar-component"><a class="link-wrapper" href="/user/profile/fixture-user">我</a></li>` +
	`<a href="/user/profile/author-in-feed">作者</a>` +
	`</div></div></body></html>`

const dialogClosedFixtureHTML = `<!doctype html><html><body><div id="global" data-logged="0">` +
	`<div id="app"></div><div class="main-container">` +
	`<a id="login-btn" href="/login">登录</a>` +
	`<img class="xhs-download-attribution-qr-image" alt="二维码" src="` + downloadQrSrc + `">` +
	`</div></div></body></html>`

const expiredQrFixtureHTML = `<!doctype html><html><body><div id="global" data-logged="0">` +
	`<div id="app"></div><div class="main-container">` +
	`<div class="login-container"><p>二维码已过期</p></div>` +
	`<a id="login-btn" href="/login">登录</a>` +
	`</div></div></body></html>`

const attributeOnlyFixtureHTML = `<!doctype html><html><body>` +
	`<div id="global" data-logged="1"><div id="app"></div>` +
	`<a id="login-btn" href="/login">登录</a>` +
	`<img class="xhs-download-attribution-qr-image" src="` + downloadQrSrc + `">` +
	`</div></body></html>`

func TestObserveLoginFixtures(t *testing.T) {
	b := newSearchFixtureBrowser(t)
	cases := []struct {
		name    string
		html    string
		kind    string
		blocked error
	}{
		{name: "非游客账号入口", html: loggedInFixtureHTML, kind: loginKindAccount},
		{name: "游客和信息流链接", html: guestFixtureHTML, kind: loginKindGuest},
		{name: "登录框关闭仍未登录", html: dialogClosedFixtureHTML, kind: loginKindEntry},
		{name: "只有下载码", html: downloadQrOnlyHTML, kind: loginKindDownload},
		{name: "二维码过期", html: expiredQrFixtureHTML, kind: loginKindExpired},
		{name: "只有 data-logged 不算登录", html: attributeOnlyFixtureHTML, kind: loginKindEntry},
		{name: "访问限制", html: accessLimitHTML, kind: loginKindBlocked, blocked: errPageAccessLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := b.MustPage("about:blank")
			t.Cleanup(func() { _ = page.Close() })
			serveExploreFixture(t, page, tc.html, nil)
			require.NoError(t, page.Timeout(5*time.Second).Navigate(exploreFixtureURL))
			require.NoError(t, page.Timeout(5*time.Second).WaitLoad())

			view, err := observeLogin(page)
			require.NoError(t, err)
			require.Equal(t, tc.kind, view.Kind)
			if tc.blocked != nil {
				require.ErrorIs(t, view.Err, tc.blocked)
			}
		})
	}
}

func TestCheckLoginStatusFixtures(t *testing.T) {
	b := newSearchFixtureBrowser(t)

	t.Run("新页面能读到账号入口", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, loggedInFixtureHTML, nil)
		ok, err := NewLogin(page).CheckLoginStatus(context.Background())
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("未登录返回 false", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, dialogClosedFixtureHTML, nil)
		ok, err := NewLogin(page).CheckLoginStatus(context.Background())
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("验证页返回错误", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveNavigation(t, page, func(h *rod.Hijack) {
			switch {
			case strings.HasPrefix(h.Request.URL().String(), exploreFixtureURL):
				h.Response.Payload().ResponseCode = 302
				h.Response.SetHeader("Location", captchaFixtureURL)
				h.Response.SetBody("")
			default:
				h.Response.SetBody(loggedInFixtureHTML)
			}
		})
		ok, err := NewLogin(page).CheckLoginStatus(context.Background())
		require.ErrorIs(t, err, xhserrors.ErrVerificationRequired)
		require.False(t, ok)
	})
}

func TestWaitForLoginFixtures(t *testing.T) {
	b := newSearchFixtureBrowser(t)

	t.Run("出现账号入口后成功且日志不含身份", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		html := `<!doctype html><html><body><div id="global" data-logged="0"><div id="app"></div>` +
			`<div class="main-container"><div class="login-container"><img class="qrcode-img" src="` + loginQrSrc + `"></div>` +
			`</div></div><script>setTimeout(() => {` +
			`document.querySelector("#global").setAttribute("data-logged","1");` +
			`const box=document.createElement("div"); box.className="main-container";` +
			`box.innerHTML='<li class="user side-bar-component"><a class="link-wrapper" href="/user/profile/fixture-user"><span class="channel">我</span></a></li>';` +
			`document.body.append(box);}, 400);</script></body></html>`
		serveExploreFixture(t, page, html, nil)
		require.NoError(t, page.Timeout(5*time.Second).Navigate(exploreFixtureURL))
		require.NoError(t, page.Timeout(5*time.Second).WaitLoad())

		hook := &logCapture{}
		logrus.AddHook(hook)
		t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ok, err := NewLogin(page).WaitForLogin(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		text := strings.Join(hook.lines, "\n")
		require.Contains(t, text, "等待扫码确认：开始轮询账号入口")
		require.Contains(t, text, "登录成功证据：sidebar-profile-link")
		require.NotContains(t, text, "fixture-user")
		require.NotContains(t, text, loginQrSrc)
	})

	t.Run("超时取消", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, dialogClosedFixtureHTML, nil)
		require.NoError(t, page.Timeout(5*time.Second).Navigate(exploreFixtureURL))
		require.NoError(t, page.Timeout(5*time.Second).WaitLoad())

		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		defer cancel()
		start := time.Now()
		ok, err := NewLogin(page).WaitForLogin(ctx)
		require.NoError(t, err)
		require.False(t, ok)
		require.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("关掉登录框不会成功", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		var exploreNav atomic.Int32
		serveNavigation(t, page, func(h *rod.Hijack) {
			h.Response.SetHeader("Content-Type", "text/html; charset=utf-8")
			if strings.HasPrefix(h.Request.URL().String(), exploreFixtureURL) {
				exploreNav.Add(1)
			}
			h.Response.SetBody(loginDialogThenClosedHTML)
		})
		require.NoError(t, page.Timeout(5*time.Second).Navigate(exploreFixtureURL))
		require.NoError(t, page.Timeout(5*time.Second).WaitLoad())
		baseline := exploreNav.Load()

		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		ok, err := NewLogin(page).WaitForLogin(ctx)
		require.NoError(t, err)
		require.False(t, ok)
		require.Equal(t, baseline, exploreNav.Load())
	})

	t.Run("加载超过3秒后由站点跳转出现账号", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		var exploreNav atomic.Int32
		serveNavigation(t, page, func(h *rod.Hijack) {
			h.Response.SetHeader("Content-Type", "text/html; charset=utf-8")
			switch {
			case strings.HasPrefix(h.Request.URL().String(), exploreFixtureURL):
				exploreNav.Add(1)
				h.Response.SetBody(emptyFixtureHTML)
			case strings.HasPrefix(h.Request.URL().String(), rednoteFixtureURL):
				h.Response.SetBody(loginDialogThenRedirectHTML)
			case strings.HasPrefix(h.Request.URL().String(), afterScanURL):
				time.Sleep(4 * time.Second)
				h.Response.SetBody(loggedInFixtureHTML)
			default:
				h.Response.SetBody(emptyFixtureHTML)
			}
		})
		require.NoError(t, page.Timeout(5*time.Second).Navigate(rednoteFixtureURL))
		require.NoError(t, page.Timeout(5*time.Second).WaitLoad())
		baseline := exploreNav.Load()

		hook := &logCapture{}
		logrus.AddHook(hook)
		t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })

		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		start := time.Now()
		ok, err := NewLogin(page).WaitForLogin(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		require.Greater(t, time.Since(start), 3*time.Second)
		require.Equal(t, baseline, exploreNav.Load())
		text := strings.Join(hook.lines, "\n")
		require.Contains(t, text, "站点自身跳转")
		require.Contains(t, text, "不主动打开新页面")
		require.NotContains(t, text, "重新打开首页")
		require.NotContains(t, text, "程序导航")
	})

	t.Run("同页慢渲染后出现账号", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		var exploreNav atomic.Int32
		serveNavigation(t, page, func(h *rod.Hijack) {
			h.Response.SetHeader("Content-Type", "text/html; charset=utf-8")
			if strings.HasPrefix(h.Request.URL().String(), exploreFixtureURL) {
				exploreNav.Add(1)
			}
			h.Response.SetBody(slowAccountHTML)
		})
		require.NoError(t, page.Timeout(5*time.Second).Navigate(exploreFixtureURL))
		require.NoError(t, page.Timeout(5*time.Second).WaitLoad())
		baseline := exploreNav.Load()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		ok, err := NewLogin(page).WaitForLogin(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		require.Greater(t, time.Since(start), 3*time.Second)
		require.Equal(t, baseline, exploreNav.Load())
	})

	t.Run("游客跳转不能成功", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveNavigation(t, page, func(h *rod.Hijack) {
			h.Response.SetHeader("Content-Type", "text/html; charset=utf-8")
			if strings.HasPrefix(h.Request.URL().String(), afterScanURL) {
				h.Response.SetBody(guestFixtureHTML)
				return
			}
			h.Response.SetBody(loginDialogThenRedirectHTML)
		})
		require.NoError(t, page.Timeout(5*time.Second).Navigate(rednoteFixtureURL))
		require.NoError(t, page.Timeout(5*time.Second).WaitLoad())

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ok, err := NewLogin(page).WaitForLogin(ctx)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("验证限制立即停止", func(t *testing.T) {
		page := b.MustPage("about:blank")
		t.Cleanup(func() { _ = page.Close() })
		serveExploreFixture(t, page, accessLimitHTML, nil)
		require.NoError(t, page.Timeout(5*time.Second).Navigate(exploreFixtureURL))
		require.NoError(t, page.Timeout(5*time.Second).WaitLoad())

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		ok, err := NewLogin(page).WaitForLogin(ctx)
		require.False(t, ok)
		require.ErrorIs(t, err, errPageAccessLimited)
		require.Less(t, time.Since(start), 3*time.Second)
	})
}

func TestToProfilePageLoggedOutReturns(t *testing.T) {
	b := newSearchFixtureBrowser(t)
	page := b.MustPage("about:blank")
	t.Cleanup(func() { _ = page.Close() })
	serveExploreFixture(t, page, dialogClosedFixtureHTML, nil)

	start := time.Now()
	err := NewNavigate(page).ToProfilePage(context.Background())
	require.ErrorIs(t, err, errNotLoggedIn)
	require.Less(t, time.Since(start), 10*time.Second)
}

const afterScanURL = "https://www.xiaohongshu.com/after-scan"

const loginDialogThenRedirectHTML = `<!doctype html><html><body><div id="global" data-logged="0">` +
	`<div id="app"></div><div class="main-container">` +
	`<div class="login-container"><img class="qrcode-img" src="` + loginQrSrc + `"></div>` +
	`</div></div><script>setTimeout(() => { location.href = "` + afterScanURL + `"; }, 200);</script></body></html>`

const slowAccountHTML = `<!doctype html><html><body><div id="global" data-logged="0">` +
	`<div id="app"></div><div class="main-container" id="main">` +
	`<div class="login-container" id="dialog"><img class="qrcode-img" src="` + loginQrSrc + `"></div>` +
	`</div></div><script>setTimeout(() => {` +
	`document.getElementById("dialog").remove();` +
	`document.getElementById("global").setAttribute("data-logged","1");` +
	`document.getElementById("main").innerHTML = '<li class="user side-bar-component"><a class="link-wrapper" href="/user/profile/fixture-user"><span class="channel">我</span></a></li>';` +
	`}, 4000);</script></body></html>`

const loginDialogThenClosedHTML = `<!doctype html><html><body><div id="global" data-logged="0">` +
	`<div id="app"></div><div class="main-container">` +
	`<div class="login-container" id="dialog"><img class="qrcode-img" src="` + loginQrSrc + `"></div>` +
	`</div></div><script>setTimeout(() => {` +
	`document.getElementById("dialog").remove();` +
	`const a=document.createElement("a"); a.id="login-btn"; a.href="/login"; a.textContent="登录";` +
	`document.querySelector(".main-container").append(a);` +
	`}, 300);</script></body></html>`

type logCapture struct {
	lines []string
}

func (h *logCapture) Levels() []logrus.Level { return logrus.AllLevels }

func (h *logCapture) Fire(entry *logrus.Entry) error {
	h.lines = append(h.lines, entry.Message)
	return nil
}
