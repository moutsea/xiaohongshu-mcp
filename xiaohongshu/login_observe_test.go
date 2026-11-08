package xiaohongshu

import "testing"

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
