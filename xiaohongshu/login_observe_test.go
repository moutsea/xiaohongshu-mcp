package xiaohongshu

import (
	"strings"
	"testing"
)

func TestProfileUserID(t *testing.T) {
	cases := []struct {
		href string
		ok   bool
	}{
		{href: "/user/profile/fixture-user", ok: true},
		{href: "https://www.rednote.com/user/profile/fixture-user?xsec_token=secret", ok: true},
		{href: "/user/profile/", ok: false},
		{href: "/user/profile/guest", ok: false},
		{href: "/user/profile/Guest/", ok: false},
		{href: "/explore", ok: false},
		{href: "", ok: false},
	}
	for _, tc := range cases {
		id, ok := profileUserID(tc.href)
		if ok != tc.ok {
			t.Fatalf("%q ok=%v want %v", tc.href, ok, tc.ok)
		}
		if ok && (id == "" || id == "guest" || id == "secret") {
			t.Fatalf("%q id=%q", tc.href, id)
		}
	}
}

func TestSafeLocationDropsSecrets(t *testing.T) {
	host, path := safeLocation("https://user:secret@www.xiaohongshu.com/explore?xsec_token=abc#session")
	if host != "www.xiaohongshu.com" || path != "/explore" {
		t.Fatalf("explore host=%q path=%q", host, path)
	}
	host, path = safeLocation("https://www.xiaohongshu.com/user/profile/fixture-user?xsec_token=abc#h")
	if host != "www.xiaohongshu.com" || path != "/user/profile/{id}" {
		t.Fatalf("profile host=%q path=%q", host, path)
	}
	line := formatLoginDiag("login-entry", loginSurface{
		host: host, path: "/explore", container: true, loginBtn: true, dataLogged: "0",
	})
	for _, secret := range []string{"secret", "abc", "fixture-user", "xsec_token", "?"} {
		if strings.Contains(line, secret) {
			t.Fatalf("diag line contains %q: %s", secret, line)
		}
	}
	if !strings.Contains(line, "container=1") || !strings.Contains(line, "qrcode_img=0") || !strings.Contains(line, "login_btn=1") {
		t.Fatal(line)
	}
}
