package chatlog

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/chatlog/ctx"
	"github.com/sjzar/chatlog/internal/chatlog/database"
	"github.com/sjzar/chatlog/internal/chatlog/http"
	"github.com/sjzar/chatlog/internal/chatlog/wechat"
	"github.com/sjzar/chatlog/internal/tray"
	iwechat "github.com/sjzar/chatlog/internal/wechat"
	"github.com/sjzar/chatlog/pkg/config"
	"github.com/sjzar/chatlog/pkg/util"
	"github.com/sjzar/chatlog/pkg/util/dat2img"
)

const initialDecryptPollInterval = 5 * time.Second

type RunMode int

const (
	RunModeHeadless RunMode = iota
	RunModeConsole
)

type RunOptions struct {
	Mode               RunMode
	AutoOpenBrowser    bool
	AutoOpenBrowserSet bool
}

// Manager 管理聊天日志应用
type Manager struct {
	ctx *ctx.Context
	sc  *conf.ServerConfig
	scm *config.Manager

	// Services
	db     *database.Service
	http   *http.Service
	wechat *wechat.Service

	// Terminal UI
	app      *App
	trayCtrl tray.Controller

	options RunOptions

	initialDecryptOnce    sync.Once
	initialDecryptMu      sync.Mutex
	initialDecryptLastErr string

	shutdownCh     chan struct{}
	shutdownOnce   sync.Once
	shutdownReason string
}

func New() *Manager {
	return &Manager{
		options: RunOptions{
			Mode:               RunModeHeadless,
			AutoOpenBrowser:    true,
			AutoOpenBrowserSet: true,
		},
		shutdownCh: make(chan struct{}),
	}
}

func (m *Manager) SetRunOptions(opts RunOptions) {
	if opts.Mode != RunModeConsole {
		opts.Mode = RunModeHeadless
	}
	if !opts.AutoOpenBrowserSet {
		opts.AutoOpenBrowser = m.options.AutoOpenBrowser
		opts.AutoOpenBrowserSet = m.options.AutoOpenBrowserSet
	}
	m.options = opts
}

func (m *Manager) Run(configPath string) error {

	var err error
	m.ctx, err = ctx.New(configPath)
	if err != nil {
		return err
	}

	m.wechat = wechat.NewService(m.ctx)

	m.db = database.NewService(m.ctx)

	m.http = http.NewService(m.ctx, m.db, m)

	instances := m.wechat.GetWeChatInstances()
	m.ctx.SetWeChatInstances(instances)
	if len(instances) >= 1 {
		m.ctx.SwitchCurrent(instances[0])
	}

	m.startInitialDecryptWatcher()

	wantHTTP := m.ctx.HTTPEnabled || m.options.Mode == RunModeHeadless
	if wantHTTP {
		if err := m.StartService(); err != nil {
			m.stopService()
			if m.options.Mode == RunModeHeadless {
				return err
			}
			log.Err(err).Msg("failed to start HTTP service")
		}
	}

	if m.options.Mode == RunModeConsole {
		m.app = NewApp(m.ctx, m)
		return m.app.Run()
	}

	if url := m.webInterfaceURL(); url != "" {
		log.Info().Str("url", url).Msg("Chatlog web interface available")
		if m.options.AutoOpenBrowser {
			m.launchBrowser(url)
		}
	}

	if runtime.GOOS == "windows" {
		ctrl, err := tray.Start(tray.Options{
			Tooltip: "Chatlog",
			OnOpen: func() {
				if next := m.webInterfaceURL(); next != "" {
					m.launchBrowser(next)
				}
			},
			OnQuit: func() {
				m.requestShutdown("tray menu exit")
			},
		})
		if err != nil {
			log.Warn().Err(err).Msg("failed to start system tray icon")
		} else {
			m.trayCtrl = ctrl
		}
	}

	log.Info().Msg("Chatlog is running in headless mode. Press Ctrl+C to exit.")
	m.waitForShutdown()
	return nil
}

func (m *Manager) webInterfaceURL() string {
	if m.ctx == nil {
		return ""
	}
	addr := strings.TrimSpace(m.ctx.GetHTTPAddr())
	if addr == "" {
		return ""
	}
	return util.ComposeLANURL(addr)
}

func (m *Manager) launchBrowser(url string) {
	if strings.TrimSpace(url) == "" {
		return
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		log.Debug().Str("url", url).Msg("launching default browser")
		if err := util.OpenBrowser(url); err != nil {
			log.Warn().Err(err).Str("url", url).Msg("failed to open browser")
		}
	}()
}

func (m *Manager) waitForShutdown() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var reason string
	select {
	case sig := <-sigCh:
		reason = fmt.Sprintf("received signal %s", sig)
	case <-m.shutdownCh:
		reason = m.shutdownReason
		if reason == "" {
			reason = "shutdown requested"
		}
	}

	log.Info().Msgf("%s, shutting down", reason)
	m.stopTray()

	if m.wechat != nil && m.ctx != nil && m.ctx.IsAutoDecrypt() {
		if err := m.wechat.StopAutoDecrypt(); err != nil {
			log.Warn().Err(err).Msg("failed to stop auto decrypt during shutdown")
		}
	}

	if err := m.stopService(); err != nil {
		log.Warn().Err(err).Msg("failed to stop services during shutdown")
	}

	log.Info().Msg("Shutdown complete")
}

func (m *Manager) requestShutdown(reason string) {
	m.shutdownOnce.Do(func() {
		m.shutdownReason = reason
		close(m.shutdownCh)
	})
}

func (m *Manager) stopTray() {
	if m.trayCtrl == nil {
		return
	}
	m.trayCtrl.Stop()
	m.trayCtrl = nil
}

func (m *Manager) startInitialDecryptWatcher() {
	m.initialDecryptOnce.Do(func() {
		go m.initialDecryptLoop()
	})
}

func (m *Manager) initialDecryptLoop() {
	if m.tryInitialDecryptOnce() {
		return
	}

	ticker := time.NewTicker(initialDecryptPollInterval)
	defer ticker.Stop()

	for range ticker.C {
		if m.tryInitialDecryptOnce() {
			return
		}
	}
}

func (m *Manager) tryInitialDecryptOnce() bool {
	if m.ctx == nil || m.wechat == nil {
		return false
	}

	instances := m.wechat.GetWeChatInstances()
	if len(instances) == 0 {
		return false
	}

	m.ctx.SetWeChatInstances(instances)

	target := m.selectAccountForAutoDecrypt(instances)
	if target == nil {
		target = instances[0]
	}

	if m.ctx.Current == nil || m.ctx.Current.Name != target.Name || m.ctx.Current.PID != target.PID {
		m.ctx.SwitchCurrent(target)
	} else {
		m.ctx.Refresh()
	}

	if err := m.DecryptDBFiles(); err != nil {
		m.recordInitialDecryptError(err)
		return false
	}

	log.Info().Str("account", target.Name).Msg("自动解密完成")
	return true
}

func (m *Manager) selectAccountForAutoDecrypt(instances []*iwechat.Account) *iwechat.Account {
	if m.ctx.Current != nil {
		for _, inst := range instances {
			if inst.Name == m.ctx.Current.Name {
				return inst
			}
		}
	}

	if m.ctx.Account != "" {
		for _, inst := range instances {
			if inst.Name == m.ctx.Account {
				return inst
			}
		}
	}

	return nil
}

func (m *Manager) recordInitialDecryptError(err error) {
	if err == nil {
		return
	}

	msg := err.Error()
	m.initialDecryptMu.Lock()
	defer m.initialDecryptMu.Unlock()
	if msg == m.initialDecryptLastErr {
		return
	}
	m.initialDecryptLastErr = msg
	log.Warn().Err(err).Msg("自动解密失败，将继续重试")
}

func (m *Manager) Switch(info *iwechat.Account, history string) error {
	if m.ctx.AutoDecrypt {
		if err := m.StopAutoDecrypt(); err != nil {
			return err
		}
	}
	if m.ctx.HTTPEnabled {
		if err := m.stopService(); err != nil {
			return err
		}
	}
	if info != nil {
		m.ctx.SwitchCurrent(info)
	} else {
		m.ctx.SwitchHistory(history)
	}

	if m.ctx.HTTPEnabled {
		// 启动HTTP服务
		if err := m.StartService(); err != nil {
			log.Info().Err(err).Msg("启动服务失败")
			m.StopService()
		}
	}
	return nil
}

func (m *Manager) StartService() error {

	// 按依赖顺序启动服务
	if err := m.db.Start(); err != nil {
		return err
	}

	if err := m.http.Start(); err != nil {
		m.db.Stop()
		return err
	}

	// 如果是 4.0 版本，更新下 xorkey
	if m.ctx.Version == 4 {
		dat2img.SetAesKey(m.ctx.ImgKey)
		go dat2img.ScanAndSetXorKey(m.ctx.DataDir)
	}

	// 更新状态
	m.ctx.SetHTTPEnabled(true)

	return nil
}

func (m *Manager) StopService() error {
	if err := m.stopService(); err != nil {
		return err
	}

	// 更新状态
	m.ctx.SetHTTPEnabled(false)

	return nil
}

func (m *Manager) stopService() error {
	// 按依赖的反序停止服务
	var errs []error

	if err := m.http.Stop(); err != nil {
		errs = append(errs, err)
	}

	if err := m.db.Stop(); err != nil {
		errs = append(errs, err)
	}

	// 如果有错误，返回第一个错误
	if len(errs) > 0 {
		return errs[0]
	}

	return nil
}

func (m *Manager) SetHTTPAddr(text string) error {
	var addr string
	if util.IsNumeric(text) {
		addr = fmt.Sprintf("127.0.0.1:%s", text)
	} else if strings.HasPrefix(text, "http://") {
		addr = strings.TrimPrefix(text, "http://")
	} else if strings.HasPrefix(text, "https://") {
		addr = strings.TrimPrefix(text, "https://")
	} else {
		addr = text
	}
	if m.ctx != nil {
		m.ctx.SetHTTPAddr(addr)
	} else if m.sc != nil {
		m.sc.SetHTTPAddr(addr)
	}
	return nil
}

func (m *Manager) GetDataKey() error {
	if m.ctx.Current == nil {
		return fmt.Errorf("未选择任何账号")
	}
	if _, err := m.wechat.GetDataKey(m.ctx.Current); err != nil {
		return err
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()
	return nil
}

func (m *Manager) DecryptDBFiles() error {
	if m.ctx.DataKey == "" {
		if m.ctx.Current == nil {
			return fmt.Errorf("未选择任何账号")
		}
		if err := m.GetDataKey(); err != nil {
			return err
		}
	}
	if m.ctx.WorkDir == "" {
		m.ctx.WorkDir = util.DefaultWorkDir(m.ctx.Account)
	}

	if err := m.wechat.DecryptDBFiles(); err != nil {
		return err
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()
	return nil
}

func (m *Manager) StartAutoDecrypt() error {
	if m.ctx.DataKey == "" || m.ctx.DataDir == "" {
		return fmt.Errorf("请先获取密钥")
	}
	if m.ctx.WorkDir == "" {
		return fmt.Errorf("请先执行解密数据")
	}

	if err := m.wechat.StartAutoDecrypt(); err != nil {
		return err
	}

	m.ctx.SetAutoDecrypt(true)
	return nil
}

func (m *Manager) StopAutoDecrypt() error {
	if err := m.wechat.StopAutoDecrypt(); err != nil {
		return err
	}

	m.ctx.SetAutoDecrypt(false)
	return nil
}

func (m *Manager) SaveSpeechConfig(cfg *conf.SpeechConfig) error {
	if cfg == nil {
		return fmt.Errorf("speech config is nil")
	}
	if err := m.ctx.SaveSpeechConfig(cfg); err != nil {
		return err
	}
	if m.http != nil {
		m.http.ReloadSpeech()
	}
	return nil
}

func (m *Manager) RefreshSession() error {
	if m.db.GetDB() == nil {
		if err := m.db.Start(); err != nil {
			return err
		}
	}
	resp, err := m.db.GetSessions("", 1, 0)
	if err != nil {
		return err
	}
	if len(resp.Items) == 0 {
		return nil
	}
	m.ctx.LastSession = resp.Items[0].NTime
	return nil
}

func (m *Manager) CommandKey(configPath string, pid int, force bool, showXorKey bool) (string, error) {

	var err error
	m.ctx, err = ctx.New(configPath)
	if err != nil {
		return "", err
	}

	m.wechat = wechat.NewService(m.ctx)

	m.ctx.SetWeChatInstances(m.wechat.GetWeChatInstances())
	if len(m.ctx.WeChatInstances) == 0 {
		return "", fmt.Errorf("wechat process not found")
	}

	if len(m.ctx.WeChatInstances) == 1 {
		key, imgKey := m.ctx.DataKey, m.ctx.ImgKey
		if len(key) == 0 || len(imgKey) == 0 || force {
			key, imgKey, err = m.ctx.WeChatInstances[0].GetKey(context.Background())
			if err != nil {
				return "", err
			}
			m.ctx.Refresh()
			m.ctx.UpdateConfig()
		}

		result := fmt.Sprintf("Data Key: [%s]\nImage Key: [%s]", key, imgKey)
		if m.ctx.Version == 4 && showXorKey {
			if b, err := dat2img.ScanAndSetXorKey(m.ctx.DataDir); err == nil {
				result += fmt.Sprintf("\nXor Key: [0x%X]", b)
			}
		}

		return result, nil
	}
	if pid == 0 {
		str := "Select a process:\n"
		for _, ins := range m.ctx.WeChatInstances {
			str += fmt.Sprintf("PID: %d. %s[Version: %s Data Dir: %s ]\n", ins.PID, ins.Name, ins.FullVersion, ins.DataDir)
		}
		return str, nil
	}
	for _, ins := range m.ctx.WeChatInstances {
		if ins.PID == uint32(pid) {
			key, imgKey := ins.Key, ins.ImgKey
			if len(key) == 0 || len(imgKey) == 0 || force {
				key, imgKey, err = ins.GetKey(context.Background())
				if err != nil {
					return "", err
				}
				m.ctx.Refresh()
				m.ctx.UpdateConfig()
			}
			result := fmt.Sprintf("Data Key: [%s]\nImage Key: [%s]", key, imgKey)
			if m.ctx.Version == 4 && showXorKey {
				if b, err := dat2img.ScanAndSetXorKey(m.ctx.DataDir); err == nil {
					result += fmt.Sprintf("\nXor Key: [0x%X]", b)
				}
			}
			return result, nil
		}
	}
	return "", fmt.Errorf("wechat process not found")
}

func (m *Manager) CommandDecrypt(configPath string, cmdConf map[string]any) error {

	var err error
	m.sc, m.scm, err = conf.LoadServiceConfig(configPath, cmdConf)
	if err != nil {
		return err
	}

	dataDir := m.sc.GetDataDir()
	if len(dataDir) == 0 {
		return fmt.Errorf("dataDir is required")
	}

	dataKey := m.sc.GetDataKey()
	if len(dataKey) == 0 {
		return fmt.Errorf("dataKey is required")
	}

	m.wechat = wechat.NewService(m.sc)

	if err := m.wechat.DecryptDBFiles(); err != nil {
		return err
	}

	return nil
}

func (m *Manager) CommandHTTPServer(configPath string, cmdConf map[string]any) error {

	var err error
	m.sc, m.scm, err = conf.LoadServiceConfig(configPath, cmdConf)
	if err != nil {
		return err
	}

	dataDir := m.sc.GetDataDir()
	workDir := m.sc.GetWorkDir()
	if len(dataDir) == 0 && len(workDir) == 0 {
		return fmt.Errorf("dataDir or workDir is required")
	}

	dataKey := m.sc.GetDataKey()
	if len(dataKey) == 0 {
		return fmt.Errorf("dataKey is required")
	}

	// 如果是 4.0 版本，处理图片密钥
	version := m.sc.GetVersion()
	if version == 4 && len(dataDir) != 0 {
		dat2img.SetAesKey(m.sc.GetImgKey())
		go dat2img.ScanAndSetXorKey(dataDir)
	}

	log.Info().Msgf("server config: %+v", m.sc)

	m.wechat = wechat.NewService(m.sc)

	m.db = database.NewService(m.sc)

	m.http = http.NewService(m.sc, m.db, m)

	if m.sc.GetAutoDecrypt() {
		if err := m.wechat.StartAutoDecrypt(); err != nil {
			return err
		}
		log.Info().Msg("auto decrypt is enabled")
	}

	// init db
	go func() {
		// 如果工作目录为空，则解密数据
		if entries, err := os.ReadDir(workDir); err == nil && len(entries) == 0 {
			log.Info().Msgf("work dir is empty, decrypt data.")
			m.db.SetDecrypting()
			if err := m.wechat.DecryptDBFiles(); err != nil {
				log.Info().Msgf("decrypt data failed: %v", err)
				return
			}
			log.Info().Msg("decrypt data success")
		}

		// 按依赖顺序启动服务
		if err := m.db.Start(); err != nil {
			log.Info().Msgf("start db failed, try to decrypt data.")
			m.db.SetDecrypting()
			if err := m.wechat.DecryptDBFiles(); err != nil {
				log.Info().Msgf("decrypt data failed: %v", err)
				return
			}
			log.Info().Msg("decrypt data success")
			if err := m.db.Start(); err != nil {
				log.Info().Msgf("start db failed: %v", err)
				m.db.SetError(err.Error())
				return
			}
		}
	}()

	return m.http.ListenAndServe()
}
