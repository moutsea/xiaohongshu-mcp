package xiaohongshu

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	exploreURL = "https://www.xiaohongshu.com/explore"
	// 导航和 load 共用这一段，避免两段各算一次。
	exploreNavTimeout = 15 * time.Second
	exploreSettle     = 2 * time.Second
	// 侧栏登录入口。公开页面 #login-btn 文案是「登录」。
	loginEntryTimeout = 8 * time.Second
	// 等登录框里的二维码。扫码本身的等待在 WaitForLogin 里，不受这个限制。
	qrcodeWaitTimeout = 15 * time.Second
	// 导航、入口、等二维码加起来仍低于宿主 60 秒。
	fetchQrcodeBudget = 50 * time.Second
)

// 公开页面上的登录二维码。点开登录框后出现；App 下载码不在这个节点里。
const loginQrcodeSelector = ".login-container .qrcode-img"

// 侧栏可见登录入口。href 指向 /login，页面自己的点击处理会拦住跳转并打开登录框。
const loginEntrySelector = "#login-btn"

// 访问限制组件。里面的二维码是限制提示，不是登录码。
const accessLimitSelector = ".access-wrapper, .access-container"

// 已登录侧栏入口。公开页面只在 loggedIn 时渲染，href 指向 /user/profile/<id>。
const accountLinkSelector = "div.main-container li.user.side-bar-component a.link-wrapper"

const (
	loginKindAccount  = "sidebar-profile-link"
	loginKindGuest    = "guest"
	loginKindEntry    = "login-entry"
	loginKindDialog   = "login-dialog"
	loginKindDownload = "download-qr"
	loginKindExpired  = "qr-expired"
	loginKindBlocked  = "blocked"
	loginKindLoading  = "loading"
)

var errPageAccessLimited = errors.New("页面出现访问限制，已停止")
var errNotLoggedIn = errors.New("未登录，无法打开个人主页")

// loginObservation 是当前公开页面的登录判断，不是 cookie 或内部接口的结果。
type loginObservation struct {
	Kind string
	Err  error
	Link *rod.Element
}

type LoginAction struct {
	page *rod.Page
}

func NewLogin(page *rod.Page) *LoginAction {
	return &LoginAction{page: page}
}

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	pp := a.page.Context(ctx)
	if err := navigateExplore(pp, "检查登录状态"); err != nil {
		return false, err
	}
	if err := settle(pp.GetContext()); err != nil {
		return false, err
	}
	view, err := observeLogin(pp)
	if err != nil {
		return false, err
	}
	if view.Kind == loginKindBlocked {
		return false, view.Err
	}
	loggedIn := view.Kind == loginKindAccount
	logrus.Infof("检查登录状态：%s", view.Kind)
	return loggedIn, nil
}

// CurrentUser 当前登录用户的基础信息。
type CurrentUser struct {
	Nickname string `json:"nickname"`
	UserID   string `json:"userId"`
}

// CurrentUser 从当前页面的 __INITIAL_STATE__ 读取登录用户信息。
// 需在 CheckLoginStatus 之后调用：复用已加载的 explore 页，不做额外导航。
func (a *LoginAction) CurrentUser(ctx context.Context) (*CurrentUser, error) {
	pp := a.page.Context(ctx).Timeout(10 * time.Second)

	res, err := pp.Eval(`() => {
		const u = window.__INITIAL_STATE__ && window.__INITIAL_STATE__.user;
		const info = u && u.userInfo && u.userInfo.value !== undefined ? u.userInfo.value : (u && u.userInfo);
		if (!info || info.guest) return "";
		return JSON.stringify({nickname: info.nickname, userId: info.userId || info.user_id});
	}`)
	if err != nil {
		return nil, errors.Wrap(err, "read current user state failed")
	}

	raw := res.Value.String()
	if raw == "" {
		return nil, errors.New("current user not found in page state")
	}

	var user CurrentUser
	if err := json.Unmarshal([]byte(raw), &user); err != nil {
		return nil, errors.Wrap(err, "unmarshal current user failed")
	}

	return &user, nil
}

func (a *LoginAction) Login(ctx context.Context) error {
	pp := a.page.Context(ctx)

	// 导航到小红书首页，这会触发二维码弹窗
	pp.MustNavigate("https://www.xiaohongshu.com/explore").MustWaitLoad()

	time.Sleep(2 * time.Second)

	if exists, _, _ := pp.Has(".main-container .user .link-wrapper .channel"); exists {
		return nil
	}

	pp.MustElement(".main-container .user .link-wrapper .channel")

	return nil
}

func (a *LoginAction) FetchQrcodeImage(ctx context.Context) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchQrcodeBudget)
	defer cancel()
	pp := a.page.Context(ctx)

	if err := navigateExplore(pp, "获取登录二维码"); err != nil {
		return "", false, err
	}
	if err := pageBlocked(pp); err != nil {
		return "", false, err
	}
	if err := settle(pp.GetContext()); err != nil {
		return "", false, err
	}
	if err := pageBlocked(pp); err != nil {
		return "", false, err
	}

	view, err := observeLogin(pp)
	if err != nil {
		return "", false, err
	}
	if view.Kind == loginKindBlocked {
		return "", false, view.Err
	}
	if view.Kind == loginKindAccount {
		return "", true, nil
	}

	// 登录框已经开着时直接取码，不再点入口。
	if src, found, err := existingLoginQrcode(pp); err != nil || found {
		return src, false, err
	}

	// 没有现成登录框时，只点一次可见入口。
	if err := openLoginEntry(pp); err != nil {
		return "", false, err
	}
	if err := pageBlocked(pp); err != nil {
		return "", false, err
	}

	el, err := pp.Timeout(qrcodeWaitTimeout).Element(loginQrcodeSelector)
	if err != nil {
		return "", false, errors.Wrap(err, "登录框未出现登录二维码")
	}
	src, err := qrcodeSrc(el)
	return src, false, err
}

func navigateExplore(pp *rod.Page, reason string) error {
	logrus.Infof("程序导航：%s url=%s", reason, exploreURL)
	nav := pp.Timeout(exploreNavTimeout)
	if err := nav.Navigate(exploreURL); err != nil {
		return errors.Wrap(err, "open explore failed")
	}
	if err := nav.WaitLoad(); err != nil {
		return errors.Wrap(err, "explore load failed")
	}
	return nil
}

func settle(ctx context.Context) error {
	timer := time.NewTimer(exploreSettle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errors.Wrap(ctx.Err(), "等待页面稳定时被取消")
	case <-timer.C:
		return nil
	}
}

func pageBlocked(pp *rod.Page) error {
	if err := CheckPageBlocked(pp); err != nil {
		return err
	}
	ok, _, err := pp.Has(accessLimitSelector)
	if err != nil {
		return errors.Wrap(err, "check access limit failed")
	}
	if ok {
		return errPageAccessLimited
	}
	return nil
}

func existingLoginQrcode(pp *rod.Page) (string, bool, error) {
	ok, el, err := pp.Has(loginQrcodeSelector)
	if err != nil {
		return "", false, errors.Wrap(err, "check login qrcode failed")
	}
	if !ok {
		return "", false, nil
	}
	src, err := qrcodeSrc(el)
	if err != nil {
		return "", false, err
	}
	return src, true, nil
}

func openLoginEntry(pp *rod.Page) error {
	el, err := pp.Timeout(loginEntryTimeout).Element(loginEntrySelector)
	if err != nil {
		return errors.Wrap(err, "登录入口未出现")
	}
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "打开登录框失败")
	}
	return nil
}

func qrcodeSrc(el *rod.Element) (string, error) {
	src, err := el.Attribute("src")
	if err != nil {
		return "", errors.Wrap(err, "get qrcode src failed")
	}
	if src == nil || len(*src) == 0 {
		return "", errors.New("qrcode src is empty")
	}
	return *src, nil
}

// WaitForLogin 只观察当前页。扫码后的跨域跳转由站点自己完成，这里不再打开探索页。
// 只有侧栏个人主页链接算成功。登录框消失、加载中、下载码、过期二维码都不会成功。
func (a *LoginAction) WaitForLogin(ctx context.Context) (bool, error) {
	logrus.Info("等待扫码确认：开始轮询账号入口，不主动打开新页面")
	pp := a.page.Context(ctx)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastKind, lastHost string
	noteHost := func() {
		host := pageHost(pp)
		if host == "" || host == lastHost {
			return
		}
		if lastHost != "" {
			logrus.Infof("站点自身跳转：%s -> %s", lastHost, host)
		}
		lastHost = host
	}
	for {
		if ctx.Err() != nil {
			logrus.Infof("等待扫码确认结束：%s，最后状态=%s", ctx.Err(), lastKind)
			return false, nil
		}
		noteHost()
		view, err := observeLogin(pp)
		noteHost()
		if err != nil {
			if ctx.Err() != nil {
				logrus.Infof("等待扫码确认结束：%s，最后状态=%s", ctx.Err(), lastKind)
				return false, nil
			}
			logrus.Errorf("读取登录状态失败：%v", err)
		} else {
			if view.Kind != lastKind {
				logrus.Infof("登录状态：%s", view.Kind)
				lastKind = view.Kind
			}
			switch view.Kind {
			case loginKindAccount:
				logrus.Info("登录成功证据：sidebar-profile-link")
				return true, nil
			case loginKindBlocked:
				logrus.Infof("登录等待停止：%v", view.Err)
				return false, view.Err
			}
		}
		select {
		case <-ctx.Done():
			logrus.Infof("等待扫码确认结束：%s，最后状态=%s", ctx.Err(), lastKind)
			return false, nil
		case <-ticker.C:
		}
	}
}

func observeLogin(pp *rod.Page) (loginObservation, error) {
	if err := pageBlocked(pp); err != nil {
		if isContextError(err) {
			return loginObservation{}, err
		}
		return loginObservation{Kind: loginKindBlocked, Err: err}, nil
	}
	logged, err := dataLogged(pp)
	if err != nil {
		return loginObservation{}, err
	}
	link, ok, err := accountLink(pp)
	if err != nil {
		return loginObservation{}, err
	}
	if ok && logged != "0" {
		return loginObservation{Kind: loginKindAccount, Link: link}, nil
	}
	if ok && logged == "0" {
		return loginObservation{Kind: loginKindGuest}, nil
	}
	expired, err := loginQrExpired(pp)
	if err != nil {
		return loginObservation{}, err
	}
	if expired {
		return loginObservation{Kind: loginKindExpired}, nil
	}
	if exists, _, err := pp.Has(loginQrcodeSelector); err != nil {
		return loginObservation{}, err
	} else if exists {
		return loginObservation{Kind: loginKindDialog}, nil
	}
	if exists, _, err := pp.Has(loginEntrySelector); err != nil {
		return loginObservation{}, err
	} else if exists {
		return loginObservation{Kind: loginKindEntry}, nil
	}
	if exists, _, err := pp.Has(".xhs-download-attribution-qr-image"); err != nil {
		return loginObservation{}, err
	} else if exists {
		return loginObservation{Kind: loginKindDownload}, nil
	}
	return loginObservation{Kind: loginKindLoading}, nil
}

func accountLink(pp *rod.Page) (*rod.Element, bool, error) {
	ok, el, err := pp.Has(accountLinkSelector)
	if err != nil || !ok {
		return nil, false, err
	}
	href, err := el.Attribute("href")
	if err != nil || href == nil {
		return nil, false, err
	}
	if _, ok := profileUserID(*href); !ok {
		return nil, false, nil
	}
	return el, true, nil
}

// profileUserID 只接受 /user/profile/<id>。空 id 和 guest 不算已登录账号。
func profileUserID(href string) (string, bool) {
	u, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	const prefix = "/user/profile/"
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(id, '/'); i >= 0 {
		id = id[:i]
	}
	if id == "" || strings.EqualFold(id, "guest") {
		return "", false
	}
	return id, true
}

func dataLogged(pp *rod.Page) (string, error) {
	ok, el, err := pp.Has("#global")
	if err != nil || !ok {
		return "", err
	}
	value, err := el.Attribute("data-logged")
	if err != nil || value == nil {
		return "", err
	}
	return *value, nil
}

func loginQrExpired(pp *rod.Page) (bool, error) {
	ok, el, err := pp.Has(".login-container")
	if err != nil || !ok {
		return false, err
	}
	text, err := el.Text()
	if err != nil {
		return false, err
	}
	return strings.Contains(text, "二维码已过期") || strings.Contains(text, "qrcode过期"), nil
}

func isContextError(err error) bool {
	return goerrors.Is(err, context.Canceled) || goerrors.Is(err, context.DeadlineExceeded)
}

func pageHost(pp *rod.Page) string {
	frameTree, err := (proto.PageGetFrameTree{}).Call(pp.Timeout(2 * time.Second))
	if err != nil || frameTree == nil || frameTree.FrameTree == nil || frameTree.FrameTree.Frame == nil {
		return ""
	}
	u, err := url.Parse(frameTree.FrameTree.Frame.URL)
	if err != nil {
		return ""
	}
	return u.Host
}
