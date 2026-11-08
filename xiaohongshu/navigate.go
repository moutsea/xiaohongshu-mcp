package xiaohongshu

import (
	"context"
	"time"

	"github.com/go-rod/rod"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

type NavigateAction struct {
	page *rod.Page
}

func NewNavigate(page *rod.Page) *NavigateAction {
	return &NavigateAction{page: page}
}

func (n *NavigateAction) ToExplorePage(ctx context.Context) error {
	page := n.page.Context(ctx).Timeout(60 * time.Second) // 加超时保护，避免 MustNavigate/MustWaitStable 无限挂

	logrus.Info("程序导航：查看个人主页前打开搜索页")
	page.MustNavigate(loginPageURL).
		MustWaitLoad().
		MustElement(`div#app`)

	return nil
}

func (n *NavigateAction) ToProfilePage(ctx context.Context) error {
	if err := n.ToExplorePage(ctx); err != nil {
		return err
	}

	// 未登录时立即返回。不要在缺失的侧栏入口上耗掉整段 60 秒再 panic。
	page := n.page.Context(ctx).Timeout(15 * time.Second)
	view, err := observeLogin(page)
	if err != nil {
		return err
	}
	if view.Kind == loginKindBlocked {
		return view.Err
	}
	if view.Kind != loginKindAccount || view.Link == nil {
		return errNotLoggedIn
	}

	humanize.Delay(ctx, humanize.BeforeClick)
	if err := humanize.Click(view.Link); err != nil {
		return errors.Wrap(err, "打开个人主页失败")
	}
	if err := page.WaitLoad(); err != nil {
		return errors.Wrap(err, "个人主页加载失败")
	}
	return nil
}
