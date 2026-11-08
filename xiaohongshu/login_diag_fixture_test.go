//go:build integration

package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

const diagPixel = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

func TestLoginBoxStructureFixtures(t *testing.T) {
	t.Setenv("XHS_LOGIN_DIAG", "1")
	b := newSearchFixtureBrowser(t)
	page := b.MustPage("about:blank")
	t.Cleanup(func() { _ = page.Close() })
	cases := []struct {
		name string
		html string
		want []string
	}{
		{
			name: "没有图",
			html: `<div class="login-container"><div>登录后查看搜索结果</div></div>`,
			want: []string{"img=0", "canvas=0", "svg=0", "qrcode_class=0", "src_set=0", "decoded=0"},
		},
		{
			name: "不同class",
			html: `<div class="login-container"><img class="other-pic" src="` + diagPixel + `"></div>`,
			want: []string{"img=1", "qrcode_class=0", "src_set=1"},
		},
		{
			name: "dataURL",
			html: `<div class="login-container"><img class="qrcode-img" src="` + diagPixel + `"></div>`,
			want: []string{"img=1", "qrcode_class=1", "src_set=1", "decoded=1"},
		},
		{
			name: "canvas",
			html: `<div class="login-container"><canvas width="20" height="20"></canvas></div>`,
			want: []string{"img=0", "canvas=1", "qrcode_class=0", "decoded=0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, page.SetDocumentContent("<!doctype html><html><body>"+tc.html+"</body></html>"))
			if strings.Contains(tc.html, "<img") {
				require.Eventually(t, func() bool {
					el, err := page.Sleeper(rod.NotFoundSleeper).Element(".login-container img")
					if err != nil {
						return false
					}
					width, err := el.Property("naturalWidth")
					return err == nil && width.Int() > 0
				}, 2*time.Second, 50*time.Millisecond)
			}
			hook := &logCapture{}
			logrus.AddHook(hook)
			t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })
			logLoginBoxStructure(page, "open")
			text := strings.Join(hook.lines, "\n")
			for _, want := range tc.want {
				require.Contains(t, text, want)
			}
			require.NotContains(t, text, "data:image")
			require.NotContains(t, text, "base64")
			require.NotContains(t, text, "other-pic")
		})
	}
}

func TestLoginNetDiagLocalImages(t *testing.T) {
	t.Setenv("XHS_LOGIN_DIAG", "1")
	const secret = "SECRET_TOKEN_DO_NOT_LOG"
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sid="+secret)
		if r.URL.Path == "/missing.png" {
			http.Error(w, secret, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `<!doctype html><html><body><div class="login-container">
			<img class="qrcode-img" src="/missing.png?token=%s">
			<img class="dead" src="%s/nope.png?token=%s">
		</div></body></html>`, secret, deadURL, secret)
	}))
	t.Cleanup(srv.Close)

	b := newSearchFixtureBrowser(t)
	page := b.MustPage("about:blank")
	t.Cleanup(func() { _ = page.Close() })
	hook := &logCapture{}
	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })

	flush := startLoginNetDiag(page)
	require.NoError(t, page.Timeout(5*time.Second).Navigate(srv.URL))
	time.Sleep(time.Second)
	flush()

	text := strings.Join(hook.lines, "\n")
	require.Contains(t, text, "Image:")
	require.Regexp(t, `4xx=[1-9]`, text)
	require.Contains(t, text, "net::ERR_CONNECTION_REFUSED=1")
	require.NotContains(t, text, secret)
	require.NotContains(t, text, "missing.png")
	require.NotContains(t, text, "slow.png")
	require.NotContains(t, text, "Set-Cookie")
	require.NotContains(t, text, srv.URL)
}

func TestLoginNetDiagCanceledImage(t *testing.T) {
	t.Setenv("XHS_LOGIN_DIAG", "1")
	const secret = "SECRET_TOKEN_DO_NOT_LOG"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><html><body><img id="gone" src="/cancel.png?token=%s"></body></html>`, secret)
	}))
	t.Cleanup(srv.Close)

	b := newSearchFixtureBrowser(t)
	page := b.MustPage("about:blank")
	t.Cleanup(func() { _ = page.Close() })
	router := page.HijackRequests()
	router.MustAdd("*cancel.png*", func(h *rod.Hijack) {
		h.Response.Fail(proto.NetworkErrorReasonAborted)
	})
	go router.Run()
	t.Cleanup(func() { _ = router.Stop() })

	hook := &logCapture{}
	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })
	flush := startLoginNetDiag(page)
	require.NoError(t, page.Timeout(5*time.Second).Navigate(srv.URL))
	time.Sleep(time.Second)
	flush()
	text := strings.Join(hook.lines, "\n")
	require.Regexp(t, `cancel=[1-9]|net::ERR_ABORTED=[1-9]`, text)
	require.NotContains(t, text, secret)
	require.NotContains(t, text, "cancel.png")
}

func TestFetchDiagStructureOmitsSecrets(t *testing.T) {
	t.Setenv("XHS_LOGIN_DIAG", "1")
	b := newSearchFixtureBrowser(t)
	page := b.MustPage("about:blank")
	t.Cleanup(func() { _ = page.Close() })
	html := `<!doctype html><html><body><div id="global" data-logged="0"></div><script>
		setTimeout(() => {
			const box = document.createElement("div");
			box.className = "login-container";
			box.innerHTML = '<img class="other-pic" src="` + diagPixel + `">';
			document.body.append(box);
		}, 2500);
		setTimeout(() => {
			const img = document.createElement("img");
			img.className = "qrcode-img";
			img.src = "` + diagPixel + `";
			document.querySelector(".login-container").append(img);
		}, 4000);
	</script></body></html>`
	serveExploreFixture(t, page, html, nil)
	hook := &logCapture{}
	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })
	start := time.Now()
	img, loggedIn, err := NewLogin(page).FetchQrcodeImage(context.Background())
	require.NoError(t, err)
	require.False(t, loggedIn)
	require.Equal(t, diagPixel, img)
	require.Less(t, time.Since(start), 10*time.Second)
	text := strings.Join(hook.lines, "\n")
	require.Contains(t, text, "取码结构：when=open")
	require.Contains(t, text, "qrcode_class=0")
	require.NotContains(t, text, "when=timeout")
	require.NotContains(t, text, "data:image")
	require.NotContains(t, text, "base64")
	require.Contains(t, text, "取码资源：")
}
