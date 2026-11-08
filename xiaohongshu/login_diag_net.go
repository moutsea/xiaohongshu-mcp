package xiaohongshu

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
)

const loginBoxCountCap = 99

// netErrToken 只保留完整的 net::ERR_ 名称。带地址或其他句子的文本记为 other。
var netErrToken = regexp.MustCompile(`^net::ERR_[A-Z0-9_]+$`)

var loginResourceTypes = []proto.NetworkResourceType{
	proto.NetworkResourceTypeDocument,
	proto.NetworkResourceTypeStylesheet,
	proto.NetworkResourceTypeImage,
	proto.NetworkResourceTypeMedia,
	proto.NetworkResourceTypeFont,
	proto.NetworkResourceTypeScript,
	proto.NetworkResourceTypeXHR,
	proto.NetworkResourceTypeFetch,
	proto.NetworkResourceTypeOther,
}

var loginBlockedReasons = map[proto.NetworkBlockedReason]struct{}{
	proto.NetworkBlockedReasonOther:                                                   {},
	proto.NetworkBlockedReasonCsp:                                                     {},
	proto.NetworkBlockedReasonMixedContent:                                            {},
	proto.NetworkBlockedReasonOrigin:                                                  {},
	proto.NetworkBlockedReasonInspector:                                               {},
	proto.NetworkBlockedReasonSubresourceFilter:                                       {},
	proto.NetworkBlockedReasonContentType:                                             {},
	proto.NetworkBlockedReasonCoepFrameResourceNeedsCoepHeader:                        {},
	proto.NetworkBlockedReasonCoopSandboxedIframeCannotNavigateToCoopPage:             {},
	proto.NetworkBlockedReasonCorpNotSameOrigin:                                       {},
	proto.NetworkBlockedReasonCorpNotSameOriginAfterDefaultedToSameOriginByCoep:       {},
	proto.NetworkBlockedReasonCorpNotSameOriginAfterDefaultedToSameOriginByDip:        {},
	proto.NetworkBlockedReasonCorpNotSameOriginAfterDefaultedToSameOriginByCoepAndDip: {},
}

// startLoginNetDiag 只在诊断打开时监听当前页的网络事件。关闭时不做任何 CDP 调用。
// 返回的函数停止监听并写一行汇总。Image 事件为 0 不能说明没有二维码，data URL 和 canvas 不走这条事件。
func startLoginNetDiag(pp *rod.Page) func() {
	if !loginDiagOn() || pp == nil {
		return func() {}
	}
	stats := newLoginNetStats()
	ctx, cancel := context.WithCancel(pp.GetContext())
	session := pp.SessionID
	messages := pp.Browser().Context(ctx).Event()
	done := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = proto.NetworkDisable{}.Call(pp.Timeout(time.Second)) }()
		close(ready)
		for msg := range messages {
			if session != "" && msg.SessionID != session {
				continue
			}
			switch msg.Method {
			case "Network.responseReceived":
				event := &proto.NetworkResponseReceived{}
				if msg.Load(event) {
					stats.addResponse(event)
				}
			case "Network.loadingFailed":
				event := &proto.NetworkLoadingFailed{}
				if msg.Load(event) {
					stats.addFail(event)
				}
			}
		}
	}()
	<-ready
	_ = proto.NetworkEnable{}.Call(pp)
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		logrus.Info(stats.format())
	}
}

type loginNetStats struct {
	mu     sync.Mutex
	resp   map[string]map[string]int
	errN   map[string]map[string]int
	cancel map[string]int
	block  map[string]map[string]int
	any    bool
}

func newLoginNetStats() *loginNetStats {
	return &loginNetStats{
		resp:   map[string]map[string]int{},
		errN:   map[string]map[string]int{},
		cancel: map[string]int{},
		block:  map[string]map[string]int{},
	}
}

func (s *loginNetStats) addResponse(e *proto.NetworkResponseReceived) {
	if e == nil || e.Response == nil {
		return
	}
	kind := resourceKind(e.Type)
	class := statusClass(e.Response.Status)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.any = true
	s.add(s.resp, kind, class)
}

func (s *loginNetStats) addFail(e *proto.NetworkLoadingFailed) {
	if e == nil {
		return
	}
	kind := resourceKind(e.Type)
	errToken := sanitizeNetErr(e.ErrorText)
	block := sanitizeBlock(e.BlockedReason)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.any = true
	if errToken != "" {
		s.add(s.errN, kind, errToken)
	}
	if e.Canceled {
		s.cancel[kind]++
	}
	if block != "" {
		s.add(s.block, kind, block)
	}
}

func (s *loginNetStats) add(dst map[string]map[string]int, kind, token string) {
	bucket := dst[kind]
	if bucket == nil {
		bucket = map[string]int{}
		dst[kind] = bucket
	}
	if token != "other" && len(bucket) >= 8 && bucket[token] == 0 {
		token = "other"
	}
	bucket[token]++
}

func (s *loginNetStats) format() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.any {
		return "取码资源：events=0"
	}
	var parts []string
	seen := map[string]struct{}{}
	for _, kind := range loginResourceTypes {
		name := string(kind)
		seen[name] = struct{}{}
		if line := s.kindLine(name); line != "" {
			parts = append(parts, line)
		}
	}
	var extra []string
	for name := range s.resp {
		if _, ok := seen[name]; !ok {
			extra = append(extra, name)
		}
	}
	for name := range s.errN {
		if _, ok := seen[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		if line := s.kindLine(name); line != "" {
			parts = append(parts, line)
		}
	}
	if len(parts) == 0 {
		return "取码资源：events=0"
	}
	return "取码资源：" + strings.Join(parts, " ")
}

func (s *loginNetStats) kindLine(kind string) string {
	resp := s.resp[kind]
	errN := s.errN[kind]
	block := s.block[kind]
	cancel := s.cancel[kind]
	if len(resp) == 0 && len(errN) == 0 && len(block) == 0 && cancel == 0 {
		return ""
	}
	return fmt.Sprintf("%s:2xx=%d,3xx=%d,4xx=%d,5xx=%d,other=%d,err=%s,cancel=%d,block=%s",
		kind,
		resp["2xx"], resp["3xx"], resp["4xx"], resp["5xx"], resp["other"],
		formatTokens(errN), cancel, formatTokens(block))
}

func formatTokens(bucket map[string]int) string {
	if len(bucket) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(bucket))
	for key := range bucket {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, bucket[key]))
	}
	return strings.Join(parts, "|")
}

func resourceKind(kind proto.NetworkResourceType) string {
	for _, item := range loginResourceTypes {
		if kind == item {
			return string(item)
		}
	}
	return string(proto.NetworkResourceTypeOther)
}

func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "other"
	}
}

func sanitizeNetErr(text string) string {
	if netErrToken.MatchString(text) {
		return text
	}
	if text == "" {
		return ""
	}
	return "other"
}

func sanitizeBlock(reason proto.NetworkBlockedReason) string {
	if reason == "" {
		return ""
	}
	if _, ok := loginBlockedReasons[reason]; ok {
		return string(reason)
	}
	return "other"
}

// logLoginBoxStructure 只统计登录框里的节点数量和布尔值。不记录 src、class 全文或图片内容。
func logLoginBoxStructure(pp *rod.Page, when string) {
	if !loginDiagOn() || pp == nil {
		return
	}
	if when != "open" && when != "timeout" {
		when = "other"
	}
	q := pp.Sleeper(rod.NotFoundSleeper)
	imgs := boxElements(q, ".login-container img")
	canvases := boxElements(q, ".login-container canvas")
	svgs := boxElements(q, ".login-container svg")
	srcSet := false
	decoded := false
	for _, img := range imgs {
		if src, err := img.Attribute("src"); err == nil && src != nil && strings.TrimSpace(*src) != "" {
			srcSet = true
		}
		if width, err := img.Property("naturalWidth"); err == nil && width.Int() > 0 {
			decoded = true
		}
	}
	logrus.Infof(
		"取码结构：when=%s img=%d canvas=%d svg=%d qrcode_class=%d src_set=%d decoded=%d",
		when,
		capCount(len(imgs)), capCount(len(canvases)), capCount(len(svgs)),
		boolBit(hasSelector(q, ".login-container .qrcode-img")),
		boolBit(srcSet), boolBit(decoded),
	)
}

func boxElements(pp *rod.Page, selector string) rod.Elements {
	els, err := pp.Elements(selector)
	if err != nil {
		return nil
	}
	return els
}

func capCount(n int) int {
	if n > loginBoxCountCap {
		return loginBoxCountCap
	}
	return n
}
