package chatlog

import (
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/chatlog/ctx"
	"github.com/sjzar/chatlog/internal/ui/footer"
	"github.com/sjzar/chatlog/internal/ui/form"
	"github.com/sjzar/chatlog/internal/ui/help"
	"github.com/sjzar/chatlog/internal/ui/infobar"
	"github.com/sjzar/chatlog/internal/ui/menu"
	"github.com/sjzar/chatlog/internal/wechat"
	"github.com/sjzar/chatlog/pkg/util"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	RefreshInterval = 1000 * time.Millisecond
)

type settingsKey string

const (
	settingKeySpeechProvider  settingsKey = "speech_provider"
	settingKeyLocalServiceURL settingsKey = "local_service_url"
	settingKeyHTTPAddr        settingsKey = "http_addr"
	settingKeyToggleListen    settingsKey = "toggle_listen"
	settingKeyWorkDir         settingsKey = "work_dir"
	settingKeyDataDir         settingsKey = "data_dir"
	settingKeyDataKey         settingsKey = "data_key"
	settingKeyImgKey          settingsKey = "img_key"
	settingKeyOpenAIAPIKey    settingsKey = "openai_api_key"
	settingKeyOpenAIBaseURL   settingsKey = "openai_base_url"
	settingKeyOpenAIProxy     settingsKey = "openai_proxy"
	settingKeyOpenAITimeout   settingsKey = "openai_timeout"
	settingKeyWhisperModel    settingsKey = "whisper_model"
	settingKeyWhisperThreads  settingsKey = "whisper_threads"
)

type App struct {
	*tview.Application

	ctx         *ctx.Context
	m           *Manager
	stopRefresh chan struct{}

	// page
	mainPages *tview.Pages
	infoBar   *infobar.InfoBar
	tabPages  *tview.Pages
	footer    *footer.Footer

	// tab
	menu            *menu.Menu
	help            *help.Help
	settingsMenu    *menu.SubMenu
	settingsItems   []*menu.Item
	settingsItemMap map[settingsKey]*menu.Item
	activeTab       int
	tabCount        int
}

func NewApp(ctx *ctx.Context, m *Manager) *App {
	app := &App{
		ctx:             ctx,
		m:               m,
		Application:     tview.NewApplication(),
		mainPages:       tview.NewPages(),
		infoBar:         infobar.New(),
		tabPages:        tview.NewPages(),
		footer:          footer.New(),
		menu:            menu.New("主菜单"),
		help:            help.New(),
		settingsItemMap: make(map[settingsKey]*menu.Item),
	}

	app.initMenu()
	app.initSettingsTab()

	app.updateMenuItemsState()

	return app
}

func (a *App) Run() error {

	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(a.infoBar, infobar.InfoBarViewHeight, 0, false).
		AddItem(a.tabPages, 0, 1, true).
		AddItem(a.footer, 1, 1, false)

	a.mainPages.AddPage("main", flex, true, true)

	a.tabPages.
		AddPage("0", a.menu, true, true).
		AddPage("1", a.help, true, false)
	if a.settingsMenu != nil {
		a.tabPages.AddPage("2", a.settingsMenu, true, false)
		a.tabCount = 3
	} else {
		a.tabCount = 2
	}

	a.SetInputCapture(a.inputCapture)

	go a.refresh()

	if err := a.SetRoot(a.mainPages, true).EnableMouse(false).Run(); err != nil {
		return err
	}

	return nil
}

func (a *App) Stop() {
	// 添加一个通道用于停止刷新 goroutine
	if a.stopRefresh != nil {
		close(a.stopRefresh)
	}
	a.Application.Stop()
}

func (a *App) updateMenuItemsState() {
	// 查找并更新自动解密菜单项
	for _, item := range a.menu.GetItems() {
		// 更新自动解密菜单项
		if item.Index == 5 {
			if a.ctx.AutoDecrypt {
				item.Name = "停止自动解密"
				item.Description = "停止监控数据目录更新，不再自动解密新增数据"
			} else {
				item.Name = "开启自动解密"
				item.Description = "监控数据目录更新，自动解密新增数据"
			}
		}

		// 更新HTTP服务菜单项
		if item.Index == 4 {
			if a.ctx.HTTPEnabled {
				item.Name = "停止 HTTP 服务"
				item.Description = "停止本地 HTTP & MCP 服务器"
			} else {
				item.Name = "启动 HTTP 服务"
				item.Description = "启动本地 HTTP & MCP 服务器"
			}
		}

		a.refreshSettingsMenu()
	}
}

func (a *App) switchTab(step int) {
	index := (a.activeTab + step) % a.tabCount
	if index < 0 {
		index = a.tabCount - 1
	}
	a.activeTab = index
	a.tabPages.SwitchToPage(fmt.Sprint(a.activeTab))
	switch a.activeTab {
	case 0:
		a.SetFocus(a.menu)
	case 1:
		a.SetFocus(a.help)
	case 2:
		if a.settingsMenu != nil {
			a.SetFocus(a.settingsMenu)
		}
	}
}

func (a *App) focusSettingsTab() {
	if a.settingsMenu == nil {
		return
	}
	a.activeTab = 2
	a.tabPages.SwitchToPage("2")
	a.refreshSettingsMenu()
	a.SetFocus(a.settingsMenu)
}

func (a *App) refresh() {
	tick := time.NewTicker(RefreshInterval)
	defer tick.Stop()

	for {
		select {
		case <-a.stopRefresh:
			return
		case <-tick.C:
			if a.ctx.AutoDecrypt || a.ctx.HTTPEnabled {
				a.m.RefreshSession()
			}
			a.infoBar.UpdateAccount(a.ctx.Account)
			a.infoBar.UpdateBasicInfo(a.ctx.PID, a.ctx.FullVersion, a.ctx.ExePath)
			a.infoBar.UpdateStatus(a.ctx.Status)
			a.infoBar.UpdateDataKey(a.ctx.DataKey)
			a.infoBar.UpdateImageKey(a.ctx.ImgKey)
			a.infoBar.UpdatePlatform(a.ctx.Platform)
			a.infoBar.UpdateDataUsageDir(a.ctx.DataUsage, a.ctx.DataDir)
			a.infoBar.UpdateWorkUsageDir(a.ctx.WorkUsage, a.ctx.WorkDir)
			if a.ctx.LastSession.Unix() > 1000000000 {
				a.infoBar.UpdateSession(a.ctx.LastSession.Format("2006-01-02 15:04:05"))
			}
			if a.ctx.HTTPEnabled {
				addr := a.ctx.HTTPAddr
				h, _, err := net.SplitHostPort(addr)
				if err != nil { // Fallback if malformed
					a.infoBar.UpdateHTTPServer(fmt.Sprintf("[green][已启动][white] [%s]", addr))
				} else {
					h = strings.TrimSpace(h)
					if h == "0.0.0.0" || h == "::" || h == "[::]" || h == "" {
						lan := util.ComposeLANURL(addr)
						a.infoBar.UpdateHTTPServer(fmt.Sprintf("[green][已启动][white] [%s]", lan))
					} else {
						a.infoBar.UpdateHTTPServer(fmt.Sprintf("[green][已启动][white] [%s]", addr))
					}
				}
			} else {
				a.infoBar.UpdateHTTPServer("[未启动]")
			}
			if a.ctx.AutoDecrypt {
				a.infoBar.UpdateAutoDecrypt("[green][已开启][white]")
			} else {
				a.infoBar.UpdateAutoDecrypt("[未开启]")
			}

			a.Draw()
		}
	}
}

func (a *App) inputCapture(event *tcell.EventKey) *tcell.EventKey {

	// 如果当前页面不是主页面，ESC 键返回主页面
	if a.mainPages.HasPage("submenu") && event.Key() == tcell.KeyEscape {
		a.mainPages.RemovePage("submenu")
		a.mainPages.SwitchToPage("main")
		return nil
	}

	if a.tabPages.HasFocus() {
		switch event.Key() {
		case tcell.KeyLeft:
			a.switchTab(-1)
			return nil
		case tcell.KeyRight:
			a.switchTab(1)
			return nil
		}
	}

	switch event.Key() {
	case tcell.KeyCtrlC:
		a.Stop()
	}

	return event
}

func (a *App) initMenu() {
	getDataKey := &menu.Item{
		Index:       2,
		Name:        "获取密钥",
		Description: "从进程获取数据密钥 & 图片密钥",
		Selected: func(i *menu.Item) {
			modal := tview.NewModal()
			if runtime.GOOS == "darwin" {
				modal.SetText("获取密钥中...\n预计需要 20 秒左右的时间，期间微信会卡住，请耐心等待")
			} else {
				modal.SetText("获取密钥中...")
			}
			a.mainPages.AddPage("modal", modal, true, true)
			a.SetFocus(modal)

			go func() {
				err := a.m.GetDataKey()

				// 在主线程中更新UI
				a.QueueUpdateDraw(func() {
					if err != nil {
						// 解密失败
						modal.SetText("获取密钥失败: " + err.Error())
					} else {
						// 解密成功
						modal.SetText("获取密钥成功")
						a.refreshSettingsMenu()
					}

					// 添加确认按钮
					modal.AddButtons([]string{"OK"})
					modal.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
						a.mainPages.RemovePage("modal")
					})
					a.SetFocus(modal)
				})
			}()
		},
	}

	decryptData := &menu.Item{
		Index:       3,
		Name:        "解密数据",
		Description: "解密数据文件",
		Selected: func(i *menu.Item) {
			// 创建一个没有按钮的模态框，显示"解密中..."
			modal := tview.NewModal().
				SetText("解密中...")

			a.mainPages.AddPage("modal", modal, true, true)
			a.SetFocus(modal)

			// 在后台执行解密操作
			go func() {
				// 执行解密
				err := a.m.DecryptDBFiles()

				// 在主线程中更新UI
				a.QueueUpdateDraw(func() {
					if err != nil {
						// 解密失败
						modal.SetText("解密失败: " + err.Error())
					} else {
						// 解密成功
						modal.SetText("解密数据成功")
					}

					// 添加确认按钮
					modal.AddButtons([]string{"OK"})
					modal.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
						a.mainPages.RemovePage("modal")
					})
					a.SetFocus(modal)
				})
			}()
		},
	}

	httpServer := &menu.Item{
		Index:       4,
		Name:        "启动 HTTP 服务",
		Description: "启动本地 HTTP & MCP 服务器",
		Selected: func(i *menu.Item) {
			modal := tview.NewModal()

			// 根据当前服务状态执行不同操作
			if !a.ctx.HTTPEnabled {
				// HTTP 服务未启动，启动服务
				modal.SetText("正在启动 HTTP 服务...")
				a.mainPages.AddPage("modal", modal, true, true)
				a.SetFocus(modal)

				// 在后台启动服务
				go func() {
					err := a.m.StartService()

					// 在主线程中更新UI
					a.QueueUpdateDraw(func() {
						if err != nil {
							// 启动失败
							modal.SetText("启动 HTTP 服务失败: " + err.Error())
						} else {
							// 启动成功
							modal.SetText("已启动 HTTP 服务")
						}

						// 更改菜单项名称
						a.updateMenuItemsState()

						// 添加确认按钮
						modal.AddButtons([]string{"OK"})
						modal.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
							a.mainPages.RemovePage("modal")
						})
						a.SetFocus(modal)
					})
				}()
			} else {
				// HTTP 服务已启动，停止服务
				modal.SetText("正在停止 HTTP 服务...")
				a.mainPages.AddPage("modal", modal, true, true)
				a.SetFocus(modal)

				// 在后台停止服务
				go func() {
					err := a.m.StopService()

					// 在主线程中更新UI
					a.QueueUpdateDraw(func() {
						if err != nil {
							// 停止失败
							modal.SetText("停止 HTTP 服务失败: " + err.Error())
						} else {
							// 停止成功
							modal.SetText("已停止 HTTP 服务")
						}

						// 更改菜单项名称
						a.updateMenuItemsState()

						// 添加确认按钮
						modal.AddButtons([]string{"OK"})
						modal.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
							a.mainPages.RemovePage("modal")
						})
						a.SetFocus(modal)
					})
				}()
			}
		},
	}

	autoDecrypt := &menu.Item{
		Index:       5,
		Name:        "开启自动解密",
		Description: "自动解密新增的数据文件",
		Selected: func(i *menu.Item) {
			modal := tview.NewModal()

			// 根据当前自动解密状态执行不同操作
			if !a.ctx.AutoDecrypt {
				// 自动解密未开启，开启自动解密
				modal.SetText("正在开启自动解密...")
				a.mainPages.AddPage("modal", modal, true, true)
				a.SetFocus(modal)

				// 在后台开启自动解密
				go func() {
					err := a.m.StartAutoDecrypt()

					// 在主线程中更新UI
					a.QueueUpdateDraw(func() {
						if err != nil {
							// 开启失败
							modal.SetText("开启自动解密失败: " + err.Error())
						} else {
							// 开启成功
							if a.ctx.Version == 3 {
								modal.SetText("已开启自动解密\n3.x版本数据文件更新不及时，有低延迟需求请使用4.0版本")
							} else {
								modal.SetText("已开启自动解密")
							}
						}

						// 更改菜单项名称
						a.updateMenuItemsState()

						// 添加确认按钮
						modal.AddButtons([]string{"OK"})
						modal.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
							a.mainPages.RemovePage("modal")
						})
						a.SetFocus(modal)
					})
				}()
			} else {
				// 自动解密已开启，停止自动解密
				modal.SetText("正在停止自动解密...")
				a.mainPages.AddPage("modal", modal, true, true)
				a.SetFocus(modal)

				// 在后台停止自动解密
				go func() {
					err := a.m.StopAutoDecrypt()

					// 在主线程中更新UI
					a.QueueUpdateDraw(func() {
						if err != nil {
							// 停止失败
							modal.SetText("停止自动解密失败: " + err.Error())
						} else {
							// 停止成功
							modal.SetText("已停止自动解密")
						}

						// 更改菜单项名称
						a.updateMenuItemsState()

						// 添加确认按钮
						modal.AddButtons([]string{"OK"})
						modal.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
							a.mainPages.RemovePage("modal")
						})
						a.SetFocus(modal)
					})
				}()
			}
		},
	}

	setting := &menu.Item{
		Index:       6,
		Name:        "设置",
		Description: "设置应用程序选项",
		Selected: func(*menu.Item) {
			a.focusSettingsTab()
		},
	}

	selectAccount := &menu.Item{
		Index:       7,
		Name:        "切换账号",
		Description: "切换当前操作的账号，可以选择进程或历史账号",
		Selected:    a.selectAccountSelected,
	}

	a.menu.AddItem(getDataKey)
	a.menu.AddItem(decryptData)
	a.menu.AddItem(httpServer)
	a.menu.AddItem(autoDecrypt)
	a.menu.AddItem(setting)
	a.menu.AddItem(selectAccount)

	a.menu.AddItem(&menu.Item{
		Index:       8,
		Name:        "退出",
		Description: "退出程序",
		Selected: func(i *menu.Item) {
			a.Stop()
		},
	})
}

func (a *App) initSettingsTab() {
	a.settingsMenu = menu.NewSubMenu("设置")
	if a.settingsMenu == nil {
		return
	}
	a.settingsMenu.SetCancelFunc(nil)

	a.settingsItems = []*menu.Item{
		a.newSettingsItem(1, "设置语音服务提供商", settingKeySpeechProvider, a.settingSpeechProvider),
		a.newSettingsItem(2, "设置 Whisper.cpp 模型路径", settingKeyWhisperModel, a.settingWhisperModelPath),
		a.newSettingsItem(3, "设置 Whisper.cpp 线程数", settingKeyWhisperThreads, a.settingWhisperThreads),
		a.newSettingsItem(4, "设置本地语音服务地址", settingKeyLocalServiceURL, a.settingLocalServiceURL),
		a.newSettingsItem(5, "设置 HTTP 服务地址", settingKeyHTTPAddr, a.settingHTTPPort),
		a.newSettingsItem(6, "切换局域网监听", settingKeyToggleListen, a.toggleListen),
		a.newSettingsItem(7, "设置工作目录", settingKeyWorkDir, a.settingWorkDir),
		a.newSettingsItem(8, "设置数据目录", settingKeyDataDir, a.settingDataDir),
		a.newSettingsItem(9, "设置数据密钥", settingKeyDataKey, a.settingDataKey),
		a.newSettingsItem(10, "设置图片密钥", settingKeyImgKey, a.settingImgKey),
		a.newSettingsItem(11, "设置 OpenAI API Key", settingKeyOpenAIAPIKey, a.settingOpenAIAPIKey),
		a.newSettingsItem(12, "设置 OpenAI Base URL", settingKeyOpenAIBaseURL, a.settingOpenAIBaseURL),
		a.newSettingsItem(13, "设置 OpenAI 代理", settingKeyOpenAIProxy, a.settingOpenAIProxy),
		a.newSettingsItem(14, "设置 OpenAI 请求超时", settingKeyOpenAITimeout, a.settingOpenAITimeout),
	}

	a.settingsMenu.SetItems(a.settingsItems)
	a.refreshSettingsMenu()
}

func (a *App) newSettingsItem(index int, name string, key settingsKey, action func()) *menu.Item {
	item := &menu.Item{
		Index: index,
		Name:  name,
		Selected: func(*menu.Item) {
			if action != nil {
				action()
			}
		},
	}
	if a.settingsItemMap == nil {
		a.settingsItemMap = make(map[settingsKey]*menu.Item)
	}
	a.settingsItemMap[key] = item
	return item
}

func (a *App) refreshSettingsMenu() {
	if a.settingsMenu == nil || len(a.settingsItems) == 0 {
		return
	}

	speechCfg := a.ctx.GetSpeech()

	providerLabel := "OpenAI 官方服务"
	isWebService := false
	if speechCfg != nil {
		switch strings.ToLower(strings.TrimSpace(speechCfg.Provider)) {
		case "webservice", "local", "docker", "http", "whisper-asr":
			providerLabel = "本地 Docker Whisper"
			isWebService = true
		case "openai", "":
			providerLabel = "OpenAI 官方服务"
		case "whispercpp":
			providerLabel = "Whisper.cpp 本地模型"
		default:
			providerLabel = speechCfg.Provider
		}
	}

	if item := a.settingsItemMap[settingKeySpeechProvider]; item != nil {
		item.Description = fmt.Sprintf("当前提供商: %s", providerLabel)
	}

	if item := a.settingsItemMap[settingKeyWhisperModel]; item != nil {
		current := "未设置"
		if speechCfg != nil {
			trimmed := strings.TrimSpace(speechCfg.Model)
			if trimmed != "" {
				current = trimmed
			}
			if strings.ToLower(strings.TrimSpace(speechCfg.Provider)) != "whispercpp" {
				current = current + " (当前提供商未启用)"
			}
		}
		item.Description = fmt.Sprintf("当前模型路径: %s", current)
	}

	if item := a.settingsItemMap[settingKeyWhisperThreads]; item != nil {
		threadsLabel := "默认"
		if speechCfg != nil && speechCfg.Threads > 0 {
			threadsLabel = strconv.Itoa(speechCfg.Threads)
		}
		if speechCfg != nil && strings.ToLower(strings.TrimSpace(speechCfg.Provider)) != "whispercpp" {
			threadsLabel = threadsLabel + " (当前提供商未启用)"
		}
		item.Description = fmt.Sprintf("当前线程数: %s", threadsLabel)
	}

	if item := a.settingsItemMap[settingKeyLocalServiceURL]; item != nil {
		fallback := "http://127.0.0.1:9000"
		current := fallback
		if speechCfg != nil {
			current = formatPathWithFallback(speechCfg.ServiceURL, fallback)
		}
		suffix := ""
		if speechCfg != nil && !isWebService {
			suffix = " (备用)"
		}
		item.Description = fmt.Sprintf("当前服务地址: %s%s", current, suffix)
	}

	if item := a.settingsItemMap[settingKeyHTTPAddr]; item != nil {
		current := formatPathWithFallback(a.ctx.GetHTTPAddr(), "127.0.0.1:5030")
		item.Description = fmt.Sprintf("当前监听地址: %s", current)
	}

	if item := a.settingsItemMap[settingKeyToggleListen]; item != nil {
		host := a.ctx.GetHTTPAddr()
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if strings.TrimSpace(host) == "" {
			host = "127.0.0.1"
		}
		item.Description = fmt.Sprintf("当前监听主机: %s", host)
	}

	if item := a.settingsItemMap[settingKeyWorkDir]; item != nil {
		item.Description = fmt.Sprintf("当前工作目录: %s", formatPathWithFallback(a.ctx.WorkDir, "未设置"))
	}

	if item := a.settingsItemMap[settingKeyDataDir]; item != nil {
		item.Description = fmt.Sprintf("当前数据目录: %s", formatPathWithFallback(a.ctx.DataDir, "未设置"))
	}

	if item := a.settingsItemMap[settingKeyDataKey]; item != nil {
		item.Description = fmt.Sprintf("当前数据密钥: %s", formatSecretSummary(a.ctx.DataKey))
	}

	if item := a.settingsItemMap[settingKeyImgKey]; item != nil {
		item.Description = fmt.Sprintf("当前图片密钥: %s", formatSecretSummary(a.ctx.ImgKey))
	}

	if item := a.settingsItemMap[settingKeyOpenAIAPIKey]; item != nil {
		openAIKey := "未设置"
		if speechCfg != nil {
			openAIKey = formatSecretSummary(speechCfg.APIKey)
		}
		item.Description = fmt.Sprintf("当前 API Key: %s", openAIKey)
	}

	if item := a.settingsItemMap[settingKeyOpenAIBaseURL]; item != nil {
		baseURL := "未设置"
		if speechCfg != nil {
			baseURL = formatPathWithFallback(speechCfg.BaseURL, "未设置")
		}
		item.Description = fmt.Sprintf("当前 Base URL: %s", baseURL)
	}

	if item := a.settingsItemMap[settingKeyOpenAIProxy]; item != nil {
		proxy := "未设置"
		if speechCfg != nil {
			proxy = formatPathWithFallback(speechCfg.Proxy, "未设置")
		}
		item.Description = fmt.Sprintf("当前代理: %s", proxy)
	}

	if item := a.settingsItemMap[settingKeyOpenAITimeout]; item != nil {
		timeoutValue := 0
		if speechCfg != nil {
			timeoutValue = speechCfg.RequestTimeoutSeconds
		}
		item.Description = fmt.Sprintf("当前请求超时: %s", formatTimeoutSummary(timeoutValue))
	}

	a.settingsMenu.SetItems(a.settingsItems)
}

func (a *App) updateSpeechConfig(mutator func(*conf.SpeechConfig)) error {
	current := a.ctx.GetSpeech()
	cfg := conf.SpeechConfig{Enabled: true}
	if current != nil {
		cfg = *current
	} else {
		cfg.Provider = "openai"
	}

	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}

	if mutator != nil {
		mutator(&cfg)
	}

	cfg.Normalize()
	return a.m.SaveSpeechConfig(&cfg)
}

func (a *App) settingSpeechProvider() {
	buttons := []string{"OpenAI 官方服务", "本地 Docker Whisper", "Whisper.cpp 本地模型", "取消"}
	a.showModal("选择语音服务提供商", buttons, func(buttonIndex int, buttonLabel string) {
		a.mainPages.RemovePage("modal")

		var (
			provider string
			message  string
		)

		switch buttonLabel {
		case "OpenAI 官方服务":
			provider = "openai"
			message = "语音服务已切换到 OpenAI 官方服务"
		case "本地 Docker Whisper":
			provider = "webservice"
			message = "语音服务已切换到本地 Docker Whisper"
		case "Whisper.cpp 本地模型":
			provider = "whispercpp"
			message = "语音服务已切换到 Whisper.cpp 本地模型"
		default:
			return
		}

		if err := a.updateSpeechConfig(func(cfg *conf.SpeechConfig) {
			cfg.Provider = provider
			if provider == "webservice" && strings.TrimSpace(cfg.ServiceURL) == "" {
				cfg.ServiceURL = "http://127.0.0.1:9000"
			}
		}); err != nil {
			a.showError(err)
			return
		}

		a.refreshSettingsMenu()
		if message != "" {
			a.showInfo(message)
		}
	})
}

func (a *App) settingWhisperModelPath() {
	formView := form.NewForm("设置 Whisper.cpp 模型路径")

	speech := a.ctx.GetSpeech()
	currentValue := ""
	if speech != nil {
		currentValue = strings.TrimSpace(speech.Model)
	}
	tempValue := currentValue

	formView.AddInputField("模型文件路径", tempValue, 0, nil, func(text string) {
		tempValue = text
	})

	formView.AddButton("保存", func() {
		trimmed := strings.TrimSpace(tempValue)
		if trimmed != "" {
			trimmed = filepath.Clean(trimmed)
		}

		if err := a.updateSpeechConfig(func(cfg *conf.SpeechConfig) {
			cfg.Model = trimmed
		}); err != nil {
			a.showError(err)
			return
		}
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("Whisper.cpp 模型路径已更新")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

func (a *App) settingWhisperThreads() {
	formView := form.NewForm("设置 Whisper.cpp 线程数")

	speech := a.ctx.GetSpeech()
	currentValue := ""
	if speech != nil && speech.Threads > 0 {
		currentValue = strconv.Itoa(speech.Threads)
	}
	tempValue := currentValue

	acceptNumeric := func(text string, lastChar rune) bool {
		if lastChar == 0 {
			return true
		}
		return lastChar >= '0' && lastChar <= '9'
	}

	formView.AddInputField("线程数", tempValue, 0, acceptNumeric, func(text string) {
		tempValue = text
	})

	formView.AddButton("保存", func() {
		trimmed := strings.TrimSpace(tempValue)
		threads := 0
		if trimmed != "" {
			v, err := strconv.Atoi(trimmed)
			if err != nil || v < 0 {
				a.showError(fmt.Errorf("请输入合法的非负整数"))
				return
			}
			threads = v
		}

		if err := a.updateSpeechConfig(func(cfg *conf.SpeechConfig) {
			cfg.Threads = threads
		}); err != nil {
			a.showError(err)
			return
		}
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("Whisper.cpp 线程数已更新")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

func (a *App) settingLocalServiceURL() {
	formView := form.NewForm("设置本地语音服务地址")

	speech := a.ctx.GetSpeech()
	currentValue := "http://127.0.0.1:9000"
	if speech != nil {
		currentValue = formatPathWithFallback(speech.ServiceURL, currentValue)
	}

	tempValue := currentValue

	formView.AddInputField("服务地址", tempValue, 0, nil, func(text string) {
		tempValue = text
	})

	formView.AddButton("保存", func() {
		if err := a.updateSpeechConfig(func(cfg *conf.SpeechConfig) {
			cfg.ServiceURL = tempValue
		}); err != nil {
			a.showError(err)
			return
		}
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("本地语音服务地址已更新")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

func (a *App) settingOpenAIAPIKey() {
	formView := form.NewForm("设置 OpenAI API Key")
	speech := a.ctx.GetSpeech()
	currentValue := ""
	if speech != nil {
		currentValue = speech.APIKey
	}
	tempValue := currentValue

	formView.AddInputField("API Key", tempValue, 0, nil, func(text string) {
		tempValue = text
	})

	formView.AddButton("保存", func() {
		if err := a.updateSpeechConfig(func(cfg *conf.SpeechConfig) {
			cfg.APIKey = tempValue
		}); err != nil {
			a.showError(err)
			return
		}
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("OpenAI API Key 已更新")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

func (a *App) settingOpenAIBaseURL() {
	formView := form.NewForm("设置 OpenAI Base URL")
	speech := a.ctx.GetSpeech()
	currentValue := ""
	if speech != nil {
		currentValue = speech.BaseURL
	}
	tempValue := currentValue

	formView.AddInputField("Base URL", tempValue, 0, nil, func(text string) {
		tempValue = text
	})

	formView.AddButton("保存", func() {
		if err := a.updateSpeechConfig(func(cfg *conf.SpeechConfig) {
			cfg.BaseURL = tempValue
		}); err != nil {
			a.showError(err)
			return
		}
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("OpenAI Base URL 已更新")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

func (a *App) settingOpenAIProxy() {
	formView := form.NewForm("设置 OpenAI 代理")
	speech := a.ctx.GetSpeech()
	currentValue := ""
	if speech != nil {
		currentValue = speech.Proxy
	}
	tempValue := currentValue

	formView.AddInputField("代理地址", tempValue, 0, nil, func(text string) {
		tempValue = text
	})

	formView.AddButton("保存", func() {
		if err := a.updateSpeechConfig(func(cfg *conf.SpeechConfig) {
			cfg.Proxy = tempValue
		}); err != nil {
			a.showError(err)
			return
		}
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("OpenAI 代理已更新")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

func (a *App) settingOpenAITimeout() {
	formView := form.NewForm("设置 OpenAI 请求超时")
	speech := a.ctx.GetSpeech()
	currentValue := ""
	if speech != nil && speech.RequestTimeoutSeconds > 0 {
		currentValue = strconv.Itoa(speech.RequestTimeoutSeconds)
	}
	tempValue := currentValue

	acceptNumeric := func(text string, lastChar rune) bool {
		if lastChar == 0 {
			return true
		}
		return lastChar >= '0' && lastChar <= '9'
	}

	formView.AddInputField("超时(秒)", tempValue, 0, acceptNumeric, func(text string) {
		tempValue = text
	})

	formView.AddButton("保存", func() {
		trimmed := strings.TrimSpace(tempValue)
		seconds := 0
		if trimmed != "" {
			v, err := strconv.Atoi(trimmed)
			if err != nil {
				a.showError(fmt.Errorf("请输入合法的非负整数"))
				return
			}
			seconds = v
		}

		if err := a.updateSpeechConfig(func(cfg *conf.SpeechConfig) {
			cfg.RequestTimeoutSeconds = seconds
		}); err != nil {
			a.showError(err)
			return
		}
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("OpenAI 请求超时已更新")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

// settingHTTPPort 设置 HTTP 端口
func (a *App) settingHTTPPort() {
	// 使用我们的自定义表单组件
	formView := form.NewForm("设置 HTTP 地址")

	// 临时存储用户输入的值
	tempHTTPAddr := a.ctx.HTTPAddr

	// 添加输入字段 - 不再直接设置HTTP地址，而是更新临时变量
	formView.AddInputField("地址", tempHTTPAddr, 0, nil, func(text string) {
		tempHTTPAddr = text // 只更新临时变量
	})

	// 添加按钮 - 点击保存时才设置HTTP地址
	formView.AddButton("保存", func() {
		a.m.SetHTTPAddr(tempHTTPAddr) // 在这里设置HTTP地址
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("HTTP 地址已设置为 " + a.ctx.HTTPAddr)
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

// toggleListen 在 127.0.0.1 与 0.0.0.0 之间切换监听主机，保持端口不变
func (a *App) toggleListen() {
	// 计算新的地址
	cur := a.ctx.GetHTTPAddr()
	host, port, err := net.SplitHostPort(cur)
	if err != nil || port == "" {
		// 回退到默认端口
		host = "127.0.0.1"
		port = "5030"
	}
	h := strings.TrimSpace(host)
	var newHost string
	if h == "0.0.0.0" || h == "::" || h == "[::]" || h == "" {
		newHost = "127.0.0.1"
	} else {
		newHost = "0.0.0.0"
	}
	newAddr := net.JoinHostPort(newHost, port)

	// 若服务正在运行，则重启服务以应用新监听
	if a.ctx.HTTPEnabled {
		modal := tview.NewModal().SetText("正在切换监听地址...")
		a.mainPages.AddPage("modal", modal, true, true)
		a.SetFocus(modal)
		go func() {
			// 停止服务
			stopErr := a.m.StopService()
			if stopErr == nil {
				// 设置新地址
				_ = a.m.SetHTTPAddr(newAddr)
				// 启动服务
				startErr := a.m.StartService()
				a.QueueUpdateDraw(func() {
					a.mainPages.RemovePage("modal")
					if startErr != nil {
						a.showError(fmt.Errorf("切换失败: %v", startErr))
					} else {
						a.refreshSettingsMenu()
						a.showInfo("已切换监听地址为 " + newAddr)
					}
				})
				return
			}
			// 停止失败时直接报错
			a.QueueUpdateDraw(func() {
				a.mainPages.RemovePage("modal")
				a.showError(fmt.Errorf("切换失败: %v", stopErr))
			})
		}()
		return
	}

	// 服务未运行，仅更新配置
	_ = a.m.SetHTTPAddr(newAddr)
	a.refreshSettingsMenu()
	a.showInfo("已切换监听地址为 " + newAddr)
}

// settingWorkDir 设置工作目录
func (a *App) settingWorkDir() {
	// 使用我们的自定义表单组件
	formView := form.NewForm("设置工作目录")

	// 临时存储用户输入的值
	tempWorkDir := a.ctx.WorkDir

	// 添加输入字段 - 不再直接设置工作目录，而是更新临时变量
	formView.AddInputField("工作目录", tempWorkDir, 0, nil, func(text string) {
		tempWorkDir = text // 只更新临时变量
	})

	// 添加按钮 - 点击保存时才设置工作目录
	formView.AddButton("保存", func() {
		a.ctx.SetWorkDir(tempWorkDir) // 在这里设置工作目录
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("工作目录已设置为 " + a.ctx.WorkDir)
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

// settingDataKey 设置数据密钥
func (a *App) settingDataKey() {
	// 使用我们的自定义表单组件
	formView := form.NewForm("设置数据密钥")

	// 临时存储用户输入的值
	tempDataKey := a.ctx.DataKey

	// 添加输入字段 - 不直接设置数据密钥，而是更新临时变量
	formView.AddInputField("数据密钥", tempDataKey, 0, nil, func(text string) {
		tempDataKey = text // 只更新临时变量
	})

	// 添加按钮 - 点击保存时才设置数据密钥
	formView.AddButton("保存", func() {
		a.ctx.SetDataKey(tempDataKey)
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("数据密钥已设置")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

// settingImgKey 设置图片密钥 (ImgKey)
func (a *App) settingImgKey() {
	formView := form.NewForm("设置图片密钥")

	tempImgKey := a.ctx.ImgKey

	formView.AddInputField("图片密钥", tempImgKey, 0, nil, func(text string) {
		tempImgKey = text
	})

	formView.AddButton("保存", func() {
		a.ctx.SetImgKey(tempImgKey)
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("图片密钥已设置")
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

// settingDataDir 设置数据目录
func (a *App) settingDataDir() {
	// 使用我们的自定义表单组件
	formView := form.NewForm("设置数据目录")

	// 临时存储用户输入的值
	tempDataDir := a.ctx.DataDir

	// 添加输入字段 - 不直接设置数据目录，而是更新临时变量
	formView.AddInputField("数据目录", tempDataDir, 0, nil, func(text string) {
		tempDataDir = text // 只更新临时变量
	})

	// 添加按钮 - 点击保存时才设置数据目录
	formView.AddButton("保存", func() {
		a.ctx.SetDataDir(tempDataDir)
		a.mainPages.RemovePage("submenu2")
		a.refreshSettingsMenu()
		a.showInfo("数据目录已设置为 " + a.ctx.DataDir)
	})

	formView.AddButton("取消", func() {
		a.mainPages.RemovePage("submenu2")
	})

	a.mainPages.AddPage("submenu2", formView, true, true)
	a.SetFocus(formView)
}

// selectAccountSelected 处理切换账号菜单项的选择事件
func (a *App) selectAccountSelected(i *menu.Item) {
	// 创建子菜单
	subMenu := menu.NewSubMenu("切换账号")

	// 添加微信进程
	instances := a.m.wechat.GetWeChatInstances()
	if len(instances) > 0 {
		// 添加实例标题
		subMenu.AddItem(&menu.Item{
			Index:       0,
			Name:        "--- 微信进程 ---",
			Description: "",
			Hidden:      false,
			Selected:    nil,
		})

		// 添加实例列表
		for idx, instance := range instances {
			// 创建一个实例描述
			description := fmt.Sprintf("版本: %s 目录: %s", instance.FullVersion, instance.DataDir)

			// 标记当前选中的实例
			name := fmt.Sprintf("%s [%d]", instance.Name, instance.PID)
			if a.ctx.Current != nil && a.ctx.Current.PID == instance.PID {
				name = name + " [当前]"
			}

			// 创建菜单项
			instanceItem := &menu.Item{
				Index:       idx + 1,
				Name:        name,
				Description: description,
				Hidden:      false,
				Selected: func(instance *wechat.Account) func(*menu.Item) {
					return func(*menu.Item) {
						// 如果是当前账号，则无需切换
						if a.ctx.Current != nil && a.ctx.Current.PID == instance.PID {
							a.mainPages.RemovePage("submenu")
							a.showInfo("已经是当前账号")
							return
						}

						// 显示切换中的模态框
						modal := tview.NewModal().SetText("正在切换账号...")
						a.mainPages.AddPage("modal", modal, true, true)
						a.SetFocus(modal)

						// 在后台执行切换操作
						go func() {
							err := a.m.Switch(instance, "")

							// 在主线程中更新UI
							a.QueueUpdateDraw(func() {
								a.mainPages.RemovePage("modal")
								a.mainPages.RemovePage("submenu")

								if err != nil {
									// 切换失败
									a.showError(fmt.Errorf("切换账号失败: %v", err))
								} else {
									// 切换成功
									a.showInfo("切换账号成功")
									// 更新菜单状态
									a.updateMenuItemsState()
								}
							})
						}()
					}
				}(instance),
			}
			subMenu.AddItem(instanceItem)
		}
	}

	// 添加历史账号
	if len(a.ctx.History) > 0 {
		// 添加历史账号标题
		subMenu.AddItem(&menu.Item{
			Index:       100,
			Name:        "--- 历史账号 ---",
			Description: "",
			Hidden:      false,
			Selected:    nil,
		})

		// 添加历史账号列表
		idx := 101
		for account, hist := range a.ctx.History {
			// 创建一个账号描述
			description := fmt.Sprintf("版本: %s 目录: %s", hist.FullVersion, hist.DataDir)

			// 标记当前选中的账号
			name := account
			if name == "" {
				name = filepath.Base(hist.DataDir)
			}
			if a.ctx.DataDir == hist.DataDir {
				name = name + " [当前]"
			}

			// 创建菜单项
			histItem := &menu.Item{
				Index:       idx,
				Name:        name,
				Description: description,
				Hidden:      false,
				Selected: func(account string) func(*menu.Item) {
					return func(*menu.Item) {
						// 如果是当前账号，则无需切换
						if a.ctx.Current != nil && a.ctx.DataDir == a.ctx.History[account].DataDir {
							a.mainPages.RemovePage("submenu")
							a.showInfo("已经是当前账号")
							return
						}

						// 显示切换中的模态框
						modal := tview.NewModal().SetText("正在切换账号...")
						a.mainPages.AddPage("modal", modal, true, true)
						a.SetFocus(modal)

						// 在后台执行切换操作
						go func() {
							err := a.m.Switch(nil, account)

							// 在主线程中更新UI
							a.QueueUpdateDraw(func() {
								a.mainPages.RemovePage("modal")
								a.mainPages.RemovePage("submenu")

								if err != nil {
									// 切换失败
									a.showError(fmt.Errorf("切换账号失败: %v", err))
								} else {
									// 切换成功
									a.showInfo("切换账号成功")
									// 更新菜单状态
									a.updateMenuItemsState()
								}
							})
						}()
					}
				}(account),
			}
			idx++
			subMenu.AddItem(histItem)
		}
	}

	// 如果没有账号可选择
	if len(a.ctx.History) == 0 && len(instances) == 0 {
		subMenu.AddItem(&menu.Item{
			Index:       1,
			Name:        "无可用账号",
			Description: "未检测到微信进程或历史账号",
			Hidden:      false,
			Selected:    nil,
		})
	}

	// 显示子菜单
	a.mainPages.AddPage("submenu", subMenu, true, true)
	a.SetFocus(subMenu)
}

// showModal 显示一个模态对话框
func (a *App) showModal(text string, buttons []string, doneFunc func(buttonIndex int, buttonLabel string)) {
	modal := tview.NewModal().
		SetText(text).
		AddButtons(buttons).
		SetDoneFunc(doneFunc)

	a.mainPages.AddPage("modal", modal, true, true)
	a.SetFocus(modal)
}

// showError 显示错误对话框
func (a *App) showError(err error) {
	a.showModal(err.Error(), []string{"OK"}, func(buttonIndex int, buttonLabel string) {
		a.mainPages.RemovePage("modal")
	})
}

// showInfo 显示信息对话框
func (a *App) showInfo(text string) {
	a.showModal(text, []string{"OK"}, func(buttonIndex int, buttonLabel string) {
		a.mainPages.RemovePage("modal")
	})
}

func formatPathWithFallback(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

func formatSecretSummary(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "未设置"
	}
	if len(trimmed) <= 6 {
		return "已设置"
	}
	return fmt.Sprintf("已设置(长度 %d)", len(trimmed))
}

func formatTimeoutSummary(seconds int) string {
	if seconds <= 0 {
		return "默认"
	}
	return fmt.Sprintf("%d 秒", seconds)
}
