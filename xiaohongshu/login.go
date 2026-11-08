package xiaohongshu

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
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

var errPageAccessLimited = errors.New("页面出现访问限制，已停止")

type LoginAction struct {
	page *rod.Page
}

func NewLogin(page *rod.Page) *LoginAction {
	return &LoginAction{page: page}
}

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	// 加超时保护：只是查登录态的快速检查，不应无限挂（登录扫码的等待在 Login/WaitForLogin 里）
	pp := a.page.Context(ctx).Timeout(30 * time.Second)
	pp.MustNavigate("https://www.xiaohongshu.com/explore").MustWaitLoad()

	time.Sleep(1 * time.Second)

	exists, _, err := pp.Has(`.main-container .user .link-wrapper .channel`)
	if err != nil {
		return false, errors.Wrap(err, "check login status failed")
	}

	if !exists {
		return false, errors.Wrap(err, "login status element not found")
	}

	return true, nil
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

	if err := navigateExplore(pp); err != nil {
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

	if exists, _, err := pp.Has(".main-container .user .link-wrapper .channel"); err != nil {
		return "", false, errors.Wrap(err, "check login status failed")
	} else if exists {
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

func navigateExplore(pp *rod.Page) error {
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

func (a *LoginAction) WaitForLogin(ctx context.Context) bool {
	pp := a.page.Context(ctx)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			el, err := pp.Element(".main-container .user .link-wrapper .channel")
			if err == nil && el != nil {
				return true
			}
		}
	}
}
