package xiaohongshu

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	exploreURL = "https://www.xiaohongshu.com/explore"
	// 未登录时 /explore 会被站点 302 到 www.rednote.com。
	// 公开搜索页留在 www.xiaohongshu.com，游客也会出现登录框。
	loginPageURL = "https://www.xiaohongshu.com/search_result?keyword=%E5%B0%8F%E7%BA%A2%E4%B9%A6&source=web_explore_feed"
	// 导航和 load 共用这一段，避免两段各算一次。
	exploreNavTimeout = 15 * time.Second
	exploreSettle     = 2 * time.Second
	// 侧栏登录入口。公开页面 #login-btn 文案是「登录」。
	loginEntryTimeout = 8 * time.Second
	// 登录框、二维码、可见入口共用这一段，只观察一轮，不叠加、不重试。
	qrcodeWaitTimeout = 15 * time.Second
	loginObserveTick  = 200 * time.Millisecond
	// 导航加上这一轮观察仍低于宿主 60 秒。
	fetchQrcodeBudget = 50 * time.Second
	// 公开弹窗上的站点提示。出现时二维码区域是空的，不是按钮被挡住。
	siteQRLoadHint = "薯队长遇到了点小麻烦"
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
var errSiteQRLoad = errors.New("站点加载登录二维码失败")
var errNotLoggedIn = errors.New("未登录，无法打开个人主页")

type entryMiss int

const (
	entryMissNone entryMiss = iota
	entryMissCovered
	entryMissHidden
)

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

	host, path := safeLocation(frameURL(pp))
	logrus.Infof("取码页面：host=%s path=%s", dash(host), dash(path))
	var diag string
	logLoginDiag(pp, view.Kind, &diag)

	src, err := observeLoginQrcode(pp)
	return src, false, err
}

// observeLoginQrcode 在同一段时限内看登录框、二维码和可见入口。
// 登录框出现后不再点击。只点击一次真正可交互的入口。不刷新、不重新导航。
func observeLoginQrcode(pp *rod.Page) (string, error) {
	deadline := time.Now().Add(qrcodeWaitTimeout)
	clicked := false
	sawButton := false
	miss := entryMissNone
	announcedModal := false
	var diag string
	for {
		src, done, err := pollLoginQrcode(pp, &clicked, &sawButton, &miss, &announcedModal, &diag)
		if done || err != nil {
			return src, err
		}
		if !time.Now().Before(deadline) {
			break
		}
		if err := sleepObserve(pp); err != nil {
			return "", err
		}
	}
	return "", loginObserveTimeout(pp, clicked, sawButton, miss)
}

func pollLoginQrcode(pp *rod.Page, clicked, sawButton *bool, miss *entryMiss, announcedModal *bool, diag *string) (string, bool, error) {
	if err := pp.GetContext().Err(); err != nil {
		return "", true, errors.Wrap(err, "等待登录二维码时被取消")
	}
	if err := pageBlocked(pp); err != nil {
		return "", true, err
	}
	failed, err := siteQRLoadFailed(pp)
	if err != nil {
		return "", true, err
	}
	if failed {
		logrus.Info("取码停止：站点加载错误")
		return "", true, errSiteQRLoad
	}
	if src, found, err := existingLoginQrcode(pp); err != nil || found {
		if err == nil && found {
			logrus.Info("取码观察：已有二维码")
		}
		return src, true, err
	}
	modal := hasSelector(pp, ".login-container")
	if loginDiagOn() {
		kind := loginKindLoading
		if modal {
			kind = loginKindDialog
		} else if hasSelector(pp, loginEntrySelector) {
			kind = loginKindEntry
		}
		logLoginDiag(pp, kind, diag)
	}
	if modal {
		if !*announcedModal {
			logrus.Info("取码观察：登录框已在，等待二维码节点")
			*announcedModal = true
		}
		return "", false, nil
	}
	if *clicked {
		return "", false, nil
	}
	hit, buttonMiss, err := clickInteractableEntry(pp)
	if err != nil {
		return "", true, err
	}
	if hit {
		*clicked = true
		logrus.Info("取码观察：点击可见登录入口")
		return "", false, nil
	}
	if buttonMiss != entryMissNone {
		*sawButton = true
		*miss = buttonMiss
	}
	return "", false, nil
}

func loginObserveTimeout(pp *rod.Page, clicked, sawButton bool, miss entryMiss) error {
	failed, err := siteQRLoadFailed(pp)
	if err != nil {
		return err
	}
	if failed {
		logrus.Info("取码停止：站点加载错误")
		return errSiteQRLoad
	}
	if clicked || hasSelector(pp, ".login-container") {
		return errors.New("登录框未出现登录二维码")
	}
	if !sawButton {
		return errors.New("登录入口未出现")
	}
	if miss == entryMissCovered {
		return errors.New("登录入口被遮挡")
	}
	return errors.New("登录入口不可点击")
}

func siteQRLoadFailed(pp *rod.Page) (bool, error) {
	el, err := pp.Sleeper(rod.NotFoundSleeper).Element("body")
	if err != nil {
		if goerrors.Is(err, &rod.ElementNotFoundError{}) {
			return false, nil
		}
		return false, errors.Wrap(err, "读取页面提示失败")
	}
	text, err := el.Text()
	if err != nil {
		return false, errors.Wrap(err, "读取页面提示失败")
	}
	return strings.Contains(text, siteQRLoadHint), nil
}

func clickInteractableEntry(pp *rod.Page) (bool, entryMiss, error) {
	els, err := pp.Sleeper(rod.NotFoundSleeper).Elements(loginEntrySelector)
	if err != nil && !goerrors.Is(err, &rod.ElementNotFoundError{}) {
		return false, entryMissNone, errors.Wrap(err, "登录入口未出现")
	}
	miss := entryMissNone
	for _, el := range els {
		_, err := el.Timeout(400 * time.Millisecond).Interactable()
		if err == nil {
			if err := el.Timeout(3*time.Second).Click(proto.InputMouseButtonLeft, 1); err != nil {
				return false, entryMissNone, errors.Wrap(err, "打开登录框失败")
			}
			return true, entryMissNone, nil
		}
		switch {
		case goerrors.Is(err, &rod.CoveredError{}):
			if miss != entryMissHidden {
				miss = entryMissCovered
			}
		default:
			miss = entryMissHidden
		}
	}
	if len(els) == 0 {
		return false, entryMissNone, nil
	}
	return false, miss, nil
}

func sleepObserve(pp *rod.Page) error {
	timer := time.NewTimer(loginObserveTick)
	defer timer.Stop()
	select {
	case <-pp.GetContext().Done():
		return errors.Wrap(pp.GetContext().Err(), "等待登录二维码时被取消")
	case <-timer.C:
		return nil
	}
}

func navigateExplore(pp *rod.Page, reason string) error {
	host, path := safeLocation(loginPageURL)
	logrus.Infof("程序导航：%s host=%s path=%s", reason, host, path)
	nav := pp.Timeout(exploreNavTimeout)
	if err := nav.Navigate(loginPageURL); err != nil {
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
	src, err := el.Attribute("src")
	if err != nil {
		return "", false, errors.Wrap(err, "get qrcode src failed")
	}
	if src == nil || strings.TrimSpace(*src) == "" {
		return "", false, nil
	}
	return *src, true, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// WaitForLogin 只观察当前页。扫码后的跨域跳转由站点自己完成，这里不再打开探索页。
// 只有侧栏个人主页链接算成功。登录框消失、加载中、下载码、过期二维码都不会成功。
func (a *LoginAction) WaitForLogin(ctx context.Context) (bool, error) {
	logrus.Info("等待扫码确认：开始轮询账号入口，不主动打开新页面")
	pp := a.page.Context(ctx)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastKind, lastHost, lastDiag string
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
				logLoginDiag(pp, view.Kind, &lastDiag)
				return true, nil
			case loginKindBlocked:
				logrus.Infof("登录等待停止：%v", view.Err)
				logLoginDiag(pp, view.Kind, &lastDiag)
				return false, view.Err
			default:
				logLoginDiag(pp, view.Kind, &lastDiag)
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

func loginDiagOn() bool {
	return os.Getenv("XHS_LOGIN_DIAG") == "1"
}

// logLoginDiag 只在 XHS_LOGIN_DIAG=1 时记录公开结构。不记录地址参数、账号或二维码内容。
func logLoginDiag(pp *rod.Page, kind string, last *string) {
	if !loginDiagOn() {
		return
	}
	line := formatLoginDiag(kind, readLoginSurface(pp))
	if line == *last {
		return
	}
	*last = line
	logrus.Info(line)
}

type loginSurface struct {
	host           string
	path           string
	container      bool
	qrcodeImg      bool
	qrcodeSrcEmpty bool
	loginBtn       bool
	accountLink    bool
	accountHrefOK  bool
	dataLogged     string
	expired        bool
	access         bool
	downloadQR     bool
	captchaModal   bool
}

func readLoginSurface(pp *rod.Page) loginSurface {
	q := pp.Timeout(2 * time.Second)
	host, path := safeLocation(frameURL(q))
	var s loginSurface
	s.host = host
	s.path = path
	s.container = hasSelector(q, ".login-container")
	s.qrcodeImg, s.qrcodeSrcEmpty = qrcodeImgState(q)
	s.loginBtn = hasSelector(q, loginEntrySelector)
	if ok, el, err := q.Has(accountLinkSelector); err == nil && ok && el != nil {
		s.accountLink = true
		if href, herr := el.Attribute("href"); herr == nil && href != nil {
			_, s.accountHrefOK = profileUserID(*href)
		}
	}
	if logged, err := dataLogged(q); err == nil {
		switch logged {
		case "", "0", "1":
			s.dataLogged = logged
		default:
			s.dataLogged = "other"
		}
	}
	if expired, err := loginQrExpired(q); err == nil {
		s.expired = expired
	}
	s.access = hasSelector(q, accessLimitSelector)
	s.downloadQR = hasSelector(q, ".xhs-download-attribution-qr-image")
	s.captchaModal = hasSelector(q, ".r-captcha-modal")
	if s.dataLogged == "" {
		s.dataLogged = "-"
	}
	if s.host == "" {
		s.host = "-"
	}
	if s.path == "" {
		s.path = "-"
	}
	return s
}

func formatLoginDiag(kind string, s loginSurface) string {
	return fmt.Sprintf(
		"登录诊断：kind=%s host=%s path=%s container=%d qrcode_img=%d qrcode_src_empty=%d login_btn=%d account_link=%d account_href_ok=%d data_logged=%s expired=%d access=%d download_qr=%d captcha_modal=%d",
		kind, s.host, s.path,
		boolBit(s.container), boolBit(s.qrcodeImg), boolBit(s.qrcodeSrcEmpty),
		boolBit(s.loginBtn), boolBit(s.accountLink), boolBit(s.accountHrefOK),
		s.dataLogged, boolBit(s.expired), boolBit(s.access), boolBit(s.downloadQR), boolBit(s.captchaModal),
	)
}

func boolBit(v bool) int {
	if v {
		return 1
	}
	return 0
}

func hasSelector(pp *rod.Page, selector string) bool {
	ok, _, err := pp.Has(selector)
	return err == nil && ok
}

func qrcodeImgState(pp *rod.Page) (present, srcEmpty bool) {
	ok, el, err := pp.Has(loginQrcodeSelector)
	if err != nil || !ok || el == nil {
		return false, false
	}
	src, err := el.Attribute("src")
	if err != nil || src == nil || strings.TrimSpace(*src) == "" {
		return true, true
	}
	return true, false
}

// safeLocation 只留主机和路径。查询串、片段和个人主页 id 都不留下。
func safeLocation(raw string) (host, path string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" && u.Path == "" && u.EscapedPath() == "" {
		return "", ""
	}
	host = u.Hostname()
	if port := u.Port(); port != "" {
		host = u.Host
	}
	path = u.EscapedPath()
	if path == "" {
		path = "/"
	}
	const prefix = "/user/profile/"
	trimmed := strings.TrimSuffix(path, "/")
	if trimmed == "/user/profile" || strings.HasPrefix(path, prefix) {
		return host, "/user/profile/{id}"
	}
	return host, path
}

func frameURL(pp *rod.Page) string {
	frameTree, err := (proto.PageGetFrameTree{}).Call(pp.Timeout(2 * time.Second))
	if err != nil || frameTree == nil || frameTree.FrameTree == nil || frameTree.FrameTree.Frame == nil {
		return ""
	}
	return frameTree.FrameTree.Frame.URL
}

func isContextError(err error) bool {
	return goerrors.Is(err, context.Canceled) || goerrors.Is(err, context.DeadlineExceeded)
}

func pageHost(pp *rod.Page) string {
	host, _ := safeLocation(frameURL(pp))
	return host
}
