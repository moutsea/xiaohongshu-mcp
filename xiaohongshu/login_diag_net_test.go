package xiaohongshu

import (
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

func TestLoginNetStatsSanitize(t *testing.T) {
	stats := newLoginNetStats()
	stats.addFail(&proto.NetworkLoadingFailed{
		Type:      proto.NetworkResourceTypeImage,
		ErrorText: "net::ERR_CONNECTION_REFUSED http://secret.example/a?token=SECRET_TOKEN",
		Canceled:  true,
	})
	stats.addFail(&proto.NetworkLoadingFailed{
		Type:          proto.NetworkResourceTypeImage,
		ErrorText:     "net::ERR_ABORTED",
		BlockedReason: proto.NetworkBlockedReason("not-a-real-reason"),
	})
	stats.addFail(&proto.NetworkLoadingFailed{
		Type:          proto.NetworkResourceTypeImage,
		ErrorText:     "net::ERR_BLOCKED_BY_RESPONSE",
		BlockedReason: proto.NetworkBlockedReasonCsp,
	})
	stats.addResponse(&proto.NetworkResponseReceived{
		Type:     proto.NetworkResourceTypeImage,
		Response: &proto.NetworkResponse{Status: 404, URL: "http://secret.example/qr.png?token=SECRET_TOKEN"},
	})
	line := stats.format()
	for _, secret := range []string{"SECRET_TOKEN", "secret.example", "qr.png", "http://", "not-a-real-reason"} {
		if strings.Contains(line, secret) {
			t.Fatalf("日志含敏感或原始文本 %q: %s", secret, line)
		}
	}
	for _, want := range []string{"Image:", "4xx=1", "net::ERR_ABORTED=1", "cancel=1", "csp=1", "other=1", "cors=none=3"} {
		if !strings.Contains(line, want) {
			t.Fatalf("缺少 %q: %s", want, line)
		}
	}
}

func TestLoginNetStatsCorsEnum(t *testing.T) {
	stats := newLoginNetStats()
	stats.addFail(&proto.NetworkLoadingFailed{
		Type:      proto.NetworkResourceTypeFetch,
		ErrorText: "net::ERR_FAILED",
		CorsErrorStatus: &proto.NetworkCorsErrorStatus{
			CorsError:       proto.NetworkCorsErrorMissingAllowOriginHeader,
			FailedParameter: "https://secret.example SECRET_HEADER",
		},
	})
	stats.addFail(&proto.NetworkLoadingFailed{
		Type:      proto.NetworkResourceTypeFetch,
		ErrorText: "net::ERR_FAILED",
		CorsErrorStatus: &proto.NetworkCorsErrorStatus{
			CorsError:       proto.NetworkCorsError("NotARealCorsError"),
			FailedParameter: "SECRET_HEADER",
		},
	})
	stats.addFail(&proto.NetworkLoadingFailed{
		Type:      proto.NetworkResourceTypeXHR,
		ErrorText: "net::ERR_FAILED",
	})
	line := stats.format()
	for _, secret := range []string{"SECRET_HEADER", "secret.example", "https://", "NotARealCorsError", "FailedParameter"} {
		if strings.Contains(line, secret) {
			t.Fatalf("日志含原始字段 %q: %s", secret, line)
		}
	}
	if !strings.Contains(line, "Fetch:") || !strings.Contains(line, "MissingAllowOriginHeader=1") || !strings.Contains(line, "other=1") {
		t.Fatal(line)
	}
	if !strings.Contains(line, "XHR:") || !strings.Contains(line, "cors=none=1") {
		t.Fatal(line)
	}
}

func TestLoginNetStatsEmpty(t *testing.T) {
	if got := newLoginNetStats().format(); got != "取码资源：events=0" {
		t.Fatal(got)
	}
}

func TestLoginDiagOffDoesNotTouchPage(t *testing.T) {
	t.Setenv("XHS_LOGIN_DIAG", "")
	flush := startLoginNetDiag(nil)
	flush()
	logLoginBoxStructure(nil, "open")
}
