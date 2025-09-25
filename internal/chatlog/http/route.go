package http

import (
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/model"
	"github.com/sjzar/chatlog/pkg/util"
	"github.com/sjzar/chatlog/pkg/util/dat2img"
	"github.com/sjzar/chatlog/pkg/util/silk"
)

//go:embed static
var EFS embed.FS

// 统一的 HTML 预览组件片段
var previewHTMLSnippet = `
<style>#preview{position:fixed;top:60px;left:40px;z-index:9999;display:none;background:#1f2329;border:1px solid #444;padding:4px 4px 8px;border-radius:8px;max-width:720px;max-height:520px;box-shadow:0 4px 16px rgba(0,0,0,0.45);color:#eee;font-size:12px;resize:both;overflow:hidden;}#preview.dragging{opacity:.85;cursor:grabbing;}#preview .pv-header{display:flex;align-items:center;justify-content:space-between;gap:6px;margin:0 2px 4px 2px;font-size:12px;user-select:none;cursor:grab;}#preview .pv-header .title{flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;color:#9ecbff;font-weight:600;}#preview button{background:#2d333b;border:1px solid #555;color:#ddd;font-size:11px;padding:2px 6px;border-radius:4px;cursor:pointer;}#preview button:hover{background:#3a424b}#preview-content{max-width:100%;max-height:470px;overflow:auto;}#preview-content img,#preview-content video{max-width:100%;max-height:470px;display:block;border-radius:4px;}#preview-content audio{width:100%;margin-top:4px;}#preview-content .audio-meta{margin-top:4px;color:#bbb;font-size:11px;font-family:monospace;}</style>
<div id="preview"><div class="pv-header"><span class="title" id="pv-title">预览</span><button id="pv-pin" title="固定/取消固定">📌</button><button id="pv-close" title="关闭">✕</button></div><div id="preview-content"></div></div>
<script>(function(){const pv=document.getElementById('preview');const pvc=document.getElementById('preview-content');const titleEl=document.getElementById('pv-title');const pinBtn=document.getElementById('pv-pin');const closeBtn=document.getElementById('pv-close');let activeLink=null;let hideTimer=null;let pinned=false;let dragState=null;let currentType='';function esc(s){return s.replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;','\'':'&#39;'}[c]));}function build(href,text){let label=text||'';label=label.replace(/^[\[]|[\]]$/g,'');currentType='text';if(/\/image\//.test(href)){currentType='image';return '<img src="'+href+'" loading="lazy" />';}if(/\/video\//.test(href)){currentType='video';return '<video src="'+href+'" controls preload="metadata"></video>'; }if(/\/voice\//.test(href)){currentType='audio';return '<div class="audio-box"><audio src="'+href+'" controls preload="metadata"></audio><div class="audio-meta">解析中...</div></div>'; }if(/表情/.test(label)||/\.(gif|apng|webp)(\?|$)/i.test(href)){currentType='emoji';return '<img src="'+href+'" style="max-width:100%;max-height:470px;display:block;" />';}if(/\/file\//.test(href)){currentType='file';return '<div style="word-break:break-all;line-height:1.5;">文件: '+esc(label)+'<br/><a href="'+href+'" target="_blank" style="color:#61afef;">下载</a></div>'; }return '<div style="word-break:break-all;line-height:1.5;">'+esc(label)+'<br/><a href="'+href+'" target="_blank" style="color:#61afef;">打开</a></div>'; }function fmtDur(d){if(!isFinite(d)||d<=0)return '未知';const s=Math.round(d);if(s>=60){const m=Math.floor(s/60);const ss=s%60;return m+'m'+(ss<10?'0':'')+ss+'s';}return s+'s';}function parseLabelDuration(lbl){const m1=/语音\((\d+)s\)/.exec(lbl);if(m1)return m1[1]+'s';const m2=/语音\((\d+)m(\d{1,2})s\)/.exec(lbl);if(m2){const mm=m2[1],ss=m2[2];return mm+'m'+(ss.length===1?'0'+ss:ss)+'s';}return null;}function afterRender(){if(currentType==='audio'){const audio=pvc.querySelector('audio');const meta=pvc.querySelector('.audio-meta');if(audio&&meta){const label=(activeLink?activeLink.textContent:'').replace(/[\[\]]/g,'');const parsed=parseLabelDuration(label);if(parsed){meta.textContent='时长: '+parsed;}const update=()=>{if(isFinite(audio.duration)&&audio.duration>0){meta.textContent='时长: '+fmtDur(audio.duration);return true;}return false;};audio.addEventListener('loadedmetadata',()=>{update();},{once:true});let tries=0;const timer=setInterval(()=>{if(update()||++tries>6){clearInterval(timer);} },500);audio.load();}}}function adjustWidth(){if(dragState)return;const vw=window.innerWidth;const clamp=w=>Math.min(w,vw-40);switch(currentType){case'audio':pv.style.width=clamp(680)+'px';break;case'video':pv.style.width=clamp(720)+'px';break;case'file':pv.style.width=clamp(560)+'px';break;case'image':case'emoji':pv.style.width='auto';break;default:pv.style.width='420px';}}function showFor(a){clearTimeout(hideTimer);activeLink=a;const href=a.getAttribute('href');pvc.innerHTML=build(href,a.textContent||'');titleEl.textContent=a.textContent||'预览';pv.style.display='block';adjustWidth();afterRender();positionNear(a);}function positionNear(a){if(pinned||dragState)return;const rect=a.getBoundingClientRect();const pw=pv.offsetWidth;const ph=pv.offsetHeight;let x=rect.right+12;let y=rect.top;const vw=window.innerWidth;const vh=window.innerHeight;if(x+pw>vw-8)x=rect.left-pw-12;if(x<8)x=8;if(y+ph>vh-8)y=vh-ph-8;if(y<8)y=8;pv.style.left=x+'px';pv.style.top=y+'px';}function scheduleHide(){if(pinned)return;hideTimer=setTimeout(()=>{if(pinned)return;activeLink=null;pv.style.display='none';pvc.innerHTML='';},280);}document.addEventListener('mouseover',e=>{const a=e.target.closest('a.media');if(!a)return;if(a===activeLink){clearTimeout(hideTimer);return;}showFor(a);});document.addEventListener('mousemove',e=>{if(!activeLink||pinned||dragState)return;positionNear(activeLink);});pv.addEventListener('mouseenter',()=>{clearTimeout(hideTimer);});pv.addEventListener('mouseleave',()=>{scheduleHide();});document.addEventListener('mouseout',e=>{const a=e.target.closest&&e.target.closest('a.media');if(!a)return;if(pv.contains(e.relatedTarget))return;scheduleHide();});pinBtn.addEventListener('click',()=>{pinned=!pinned;pinBtn.style.opacity=pinned?1:0.6;if(!pinned){scheduleHide();}else{clearTimeout(hideTimer);}});closeBtn.addEventListener('click',()=>{pinned=false;activeLink=null;pv.style.display='none';pvc.innerHTML='';});pv.querySelector('.pv-header').addEventListener('mousedown',e=>{if(e.target===pinBtn||e.target===closeBtn)return;pinned=true;pinBtn.style.opacity=1;dragState={ox:e.clientX,oy:e.clientY,left:pv.offsetLeft,top:pv.offsetTop};pv.classList.add('dragging');e.preventDefault();});window.addEventListener('mousemove',e=>{if(!dragState)return;const dx=e.clientX-dragState.ox;const dy=e.clientY-dragState.oy;let nl=dragState.left+dx;let nt=dragState.top+dy;const vw=window.innerWidth;const vh=window.innerHeight;nl=Math.max(0,Math.min(vw-pv.offsetWidth,nl));nt=Math.max(0,Math.min(vh-pv.offsetHeight,nt));pv.style.left=nl+'px';pv.style.top=nt+'px';});window.addEventListener('mouseup',()=>{if(dragState){dragState=null;pv.classList.remove('dragging');}});window.addEventListener('keydown',e=>{if(e.key==='Escape'){pinned=false;pv.style.display='none';pvc.innerHTML='';activeLink=null;}});})();</script>`

func (s *Service) initRouter() {
	s.initBaseRouter()
	s.initMediaRouter()
	s.initAPIRouter()
	s.initMCPRouter()
}

func (s *Service) initBaseRouter() {
	staticDir, _ := fs.Sub(EFS, "static")
	s.router.StaticFS("/static", http.FS(staticDir))
	s.router.StaticFileFS("/favicon.ico", "./favicon.ico", http.FS(staticDir))
	s.router.StaticFileFS("/", "./index.htm", http.FS(staticDir))
	s.router.GET("/health", func(ctx *gin.Context) { ctx.JSON(http.StatusOK, gin.H{"status":"ok"}) })
	s.router.NoRoute(func(c *gin.Context){
		path := c.Request.URL.Path
		if strings.HasPrefix(path,"/api") || strings.HasPrefix(path,"/static") {
			c.JSON(http.StatusNotFound, gin.H{"error":"Not found"})
			return
		}
		c.Header("Cache-Control","no-cache, no-store, max-age=0, must-revalidate, value")
		c.Redirect(http.StatusFound, "/")
	})
}

func (s *Service) initMediaRouter() {
	s.router.GET("/image/*key", func(c *gin.Context) { s.handleMedia(c, "image") })
	s.router.GET("/video/*key", func(c *gin.Context) { s.handleMedia(c, "video") })
	s.router.GET("/file/*key", func(c *gin.Context) { s.handleMedia(c, "file") })
	s.router.GET("/voice/*key", func(c *gin.Context) { s.handleMedia(c, "voice") })
	s.router.GET("/data/*path", s.handleMediaData)
	s.router.GET("/avatar/:username", s.handleAvatar)
}

func (s *Service) initAPIRouter() {
	api := s.router.Group("/api/v1", s.checkDBStateMiddleware())
	{
		api.GET("/chatlog", s.handleChatlog)
		api.GET("/contact", s.handleContacts)
		api.GET("/chatroom", s.handleChatRooms)
		api.GET("/session", s.handleSessions)
		api.GET("/diary", s.handleDiary)
		api.GET("/summary", s.handleSummary)
		api.GET("/dashboard", s.handleDashboard)
	}
}

func (s *Service) initMCPRouter() {
	s.router.Any("/mcp", func(c *gin.Context) { s.mcpStreamableServer.ServeHTTP(c.Writer, c.Request) })
	s.router.Any("/sse", func(c *gin.Context) { s.mcpSSEServer.ServeHTTP(c.Writer, c.Request) })
	s.router.Any("/message", func(c *gin.Context) { s.mcpSSEServer.ServeHTTP(c.Writer, c.Request) })
}

// handleSummary outputs a dashboard summary JSON. For now it serves a template JSON
// compatible with tools/json/index.json. In future we can compute real data.
// GET /api/v1/summary
func (s *Service) handleSummary(c *gin.Context) {
	// dynamic=1 triggers dynamic summary generation; otherwise fall back to template JSON
	dynamic := c.Query("dynamic") == "1"

	// Try to load a template JSON from tools/json/index.json if present
	// Otherwise, return an empty structure with HTTP 200.
	workdir := s.conf.GetDataDir()
	candidates := []string{
		filepath.Join("tools", "json", "index.json"),
		filepath.Join(workdir, "tools", "json", "index.json"),
	}
	var raw []byte
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			raw = b
			break
		}
	}

	var v any
	if dynamic {
		// compute dynamic summary
		sum := s.computeDynamicSummary()
		v = sum
	} else {
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &v); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid summary template", "detail": err.Error()})
				return
			}
		} else {
			v = gin.H{"dashboard_report": gin.H{}}
		}
	}

	// Optional: save to file in workdir when save=1
	if c.Query("save") == "1" {
		// default filename
		filename := c.DefaultQuery("filename", "summary.json")
		if filename == "" { filename = "summary.json" }
		outPath := filepath.Join(s.conf.GetDataDir(), filename)
		if dir := filepath.Dir(outPath); dir != "." { _ = os.MkdirAll(dir, 0o755) }
		var out []byte
		var err error
		if !dynamic && len(raw) > 0 { out = raw } else { out, err = json.MarshalIndent(v, "", "  "); if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal summary", "detail": err.Error()}); return } }
		if err := os.WriteFile(outPath, out, 0o644); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save summary", "detail": err.Error()})
			return
		}
		// If download=1, stream the file as attachment
		if c.Query("download") == "1" {
			c.Header("Content-Type", "application/json")
			c.Header("Content-Disposition", "attachment; filename="+filepath.Base(outPath))
			c.Data(http.StatusOK, "application/json", out)
			return
		}
		c.JSON(http.StatusOK, gin.H{"saved": true, "path": outPath})
		return
	}

	// Optional: direct download when download=1
	if c.Query("download") == "1" {
		b := raw
		if dynamic || len(b) == 0 { var err error; b, err = json.MarshalIndent(v, "", "  "); if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal summary", "detail": err.Error()}); return } }
		c.Header("Content-Type", "application/json")
		c.Header("Content-Disposition", "attachment; filename=summary.json")
		c.Data(http.StatusOK, "application/json", b)
		return
	}

	c.JSON(http.StatusOK, v)
}

// computeDynamicSummary builds a lightweight dynamic dashboard JSON with basic metrics.
// It avoids heavy full DB scans and uses existing repository APIs for acceptable performance.
func (s *Service) computeDynamicSummary() any {
	// Sizes
	dataDir := s.conf.GetDataDir()
	workDir := dataDir // prefer dataDir for media; if database layer exposes workDir, use it
	if s.db != nil {
		if wd := s.db.GetWorkDir(); wd != "" { workDir = wd }
	}
	dirSize := safeDirSize(dataDir)
	dbSize := estimateDBSize(workDir)

	// Sessions timeline (approximate earliest/latest by NTime)
	minTime := time.Time{}
	maxTime := time.Time{}
	if sessions, err := s.db.GetSessions("", 0, 0); err == nil {
		for _, it := range sessions.Items {
			t := it.NTime
			if t.IsZero() { continue }
			if minTime.IsZero() || t.Before(minTime) { minTime = t }
			if maxTime.IsZero() || t.After(maxTime) { maxTime = t }
		}
	}

	// Contacts basic stats
	totalContacts := 0
	friends := 0
	nonFriends := 0
	if contacts, err := s.db.GetContacts("", 0, 0); err == nil {
		totalContacts = len(contacts.Items)
		for _, c := range contacts.Items { if c.IsFriend { friends++ } else { nonFriends++ } }
	}

	// Chatrooms top by member count
	roomsList := make([]map[string]any, 0)
	if rooms, err := s.db.GetChatRooms("", 0, 0); err == nil {
		for _, r := range rooms.Items {
			roomsList = append(roomsList, map[string]any{
				"name": r.Name,
				"nick": r.NickName,
				"owner": r.Owner,
				"members": len(r.Users),
			})
		}
		// simple sort: descending by members
		// do inline selection sort to avoid importing sort for tiny lists
		for i := 0; i < len(roomsList); i++ {
			maxIdx := i
			for j := i + 1; j < len(roomsList); j++ {
				if roomsList[j]["members"].(int) > roomsList[maxIdx]["members"].(int) { maxIdx = j }
			}
			if maxIdx != i { roomsList[i], roomsList[maxIdx] = roomsList[maxIdx], roomsList[i] }
		}
		if len(roomsList) > 20 { roomsList = roomsList[:20] }
	}

	// Build JSON structure
	dash := map[string]any{
		"db_stats": map[string]any{
			"db_size_mb":  roundMB(dbSize),
			"dir_size_mb": roundMB(dirSize),
		},
		"timeline": map[string]any{
			"start":  formatTime(minTime),
			"end":    formatTime(maxTime),
			"days":   diffDays(minTime, maxTime),
		},
		"contact_stats": map[string]any{
			"total": totalContacts,
			"friends": friends,
			"non_friends": nonFriends,
		},
		"group_stats": map[string]any{
			"top_member_groups": roomsList,
		},
		// Placeholders for future dynamic enrichment
		"message_stats": map[string]any{
			"total": 0,
			"by_type": map[string]int{},
		},
	}
	return map[string]any{"dashboard_report": dash}
}

// handleDashboard 返回真实统计数据的仪表盘 JSON
// GET /api/v1/dashboard
func (s *Service) handleDashboard(c *gin.Context) {
	// 基础聚合
	gstats, err := s.db.GetDB().GlobalMessageStats()
	if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error":"global stats failed", "detail": err.Error()}); return }
	groupCounts, _ := s.db.GetDB().GroupMessageCounts()
	trends, _ := s.db.GetDB().MonthlyTrend(0)
	heat, _ := s.db.GetDB().Heatmap()

	// 文件与目录大小
	dataDir := s.conf.GetDataDir()
	workDir := dataDir
	if s.db != nil { if wd := s.db.GetWorkDir(); wd != "" { workDir = wd } }
	dirSize := safeDirSize(dataDir)
	dbSize := estimateDBSize(workDir)

	// 当前账号昵称（overview.user）：优先从 WorkDir/DataDir 路径中提取 wxid_***，再用联系人 NickName 映射；找不到则回退 wxid
	extractWxid := func(p string) string {
		p = strings.TrimSpace(p)
		if p == "" { return "" }
		// 遍历路径片段，优先返回形如 wxid_ 开头的片段
		parts := strings.Split(filepath.Clean(p), string(filepath.Separator))
		for _, seg := range parts {
			if strings.HasPrefix(strings.ToLower(seg), "wxid_") {
				return seg
			}
		}
		// 兜底返回最后一段
		return filepath.Base(filepath.Clean(p))
	}

	currentUser := ""
	accountID := ""
	// 先从 WorkDir 提取（更贴近实际解密目录结构），再从 DataDir 提取
	if wd := s.db.GetWorkDir(); wd != "" && accountID == "" { accountID = extractWxid(wd) }
	if accountID == "" { accountID = extractWxid(dataDir) }

	// 若拿到候选 accountID，则尝试用联系人映射 NickName
	if accountID != "" && accountID != "." && accountID != string(filepath.Separator) {
		// Windows WeChat 4.x: v3 对应 wxid 可能带有第二段后缀，如 wxid_xxx_yyyy
		// 查找昵称时需要去掉第二个下划线及其后内容
		lookupID := accountID
		low := strings.ToLower(lookupID)
		if strings.HasPrefix(low, "wxid_") {
			// 定位第二个下划线位置
			rest := lookupID[len("wxid_"):]
			if idx := strings.Index(rest, "_"); idx >= 0 {
				lookupID = lookupID[:len("wxid_")+idx]
			}
		}
		if clist, err := s.db.GetContacts(lookupID, 0, 0); err == nil && clist != nil {
			for _, it := range clist.Items {
				if it != nil && it.UserName == lookupID {
					if strings.TrimSpace(it.NickName) != "" { currentUser = it.NickName }
					break
				}
			}
			if currentUser == "" && len(clist.Items) > 0 && clist.Items[0] != nil && clist.Items[0].UserName == lookupID {
				currentUser = clist.Items[0].NickName
			}
		}
		// 最终兜底：回退为 wxid/accountID
		if strings.TrimSpace(currentUser) == "" { currentUser = accountID }
	}

	// 联系人统计

	// 群信息（合并消息计数）
	overviewGroups := make([]map[string]any, 0)
	activeGroups := 0
	if rooms, err := s.db.GetChatRooms("", 0, 0); err == nil {
		for _, r := range rooms.Items {
			// 跳过没有 NickName 的群
			if strings.TrimSpace(r.NickName) == "" { continue }
			mc := groupCounts[r.Name]
			if mc > 0 { activeGroups++ }
			overviewGroups = append(overviewGroups, map[string]any{
				"ChatRoomName":  r.Name,
				"NickName":      r.NickName,
				"member_count":  len(r.Users),
				"message_count": mc,
			})
		}
	}

	// msgTypes 依据最新文档 + 衍生细分（文件消息 / 链接消息）补齐
	msgTypes := map[string]int64{
		"文本消息":0,
		"图片消息":0,
		"语音消息":0,
		"好友验证消息":0,
		"好友推荐消息":0,
		"聊天表情":0,
		"位置消息":0,
		"XML消息":0,      // 未细分的 49 类或其他 XML
		"文件消息":0,
		"链接消息":0,
		"音视频通话":0,
		"手机端操作消息":0,
		"系统通知":0,
		"撤回消息":0,
	}
	for k, v := range gstats.ByType { if _, ok := msgTypes[k]; ok { msgTypes[k] += v } }
	for k, v := range gstats.ByType { if _, ok := msgTypes[k]; ok { msgTypes[k] += v } }

	// 时间轴
	durationDays := 0.0
	if gstats.EarliestUnix > 0 && gstats.LatestUnix >= gstats.EarliestUnix {
		durationDays = float64(gstats.LatestUnix-gstats.EarliestUnix) / 86400.0
		durationDays = math.Round(durationDays*100) / 100.0
	}

	// trends 排序
	sort.Slice(trends, func(i, j int) bool { return trends[i].Date < trends[j].Date })
	trendData := make([]map[string]any, 0, len(trends))
	for _, t := range trends { trendData = append(trendData, map[string]any{"date": t.Date, "sent": t.Sent, "received": t.Received}) }

	// 今日每小时统计用于 most_active_hour
	perHourTotal := make([]int64, 24)
	if s.db != nil && s.db.GetDB() != nil {
		if hours, err := s.db.GetDB().GlobalTodayHourly(); err == nil {
			for i := 0; i < 24; i++ { perHourTotal[i] = hours[i] }
		}
	}
	maxHour := 0
	for h := 1; h < 24; h++ { if perHourTotal[h] > perHourTotal[maxHour] { maxHour = h } }
	mostActiveHour := fmt.Sprintf("%02d:00-%02d:00", maxHour, (maxHour+1)%24)

	// groupAnalysis（基础占位+部分真实值）
	groupList := make([]map[string]any, 0, len(overviewGroups))
	for _, g := range overviewGroups {
		groupList = append(groupList, map[string]any{
			"name":     g["NickName"],
			"members":  g["member_count"],
			"messages": g["message_count"],
			"active":   (g["message_count"].(int64) > 0),
		})
	}
	hourlyActivity := make([]map[string]any, 0, 24)
	for h := 0; h < 24; h++ { hourlyActivity = append(hourlyActivity, map[string]any{"hour": fmt.Sprintf("%02d:00", h), "messages": perHourTotal[h]}) }
	// 内容占比（基于 msgTypes）
	totalMsgs := gstats.Total
	pct := func(n int64) float64 { if totalMsgs == 0 { return 0 } ; return math.Round((float64(n)*10000.0/float64(totalMsgs)))/100.0 }
	// 私聊/群聊分布（用于 DataTypeAnalysis.SourceChannels）
	var groupTotal int64
	for _, v := range groupCounts { groupTotal += v }
	privateTotal := totalMsgs - groupTotal

	// 使用结构体固定 JSON 输出顺序
	type DBStats struct { DbSizeMB float64 `json:"db_size_mb"`; DirSizeMB float64 `json:"dir_size_mb"` }
	type MsgStats struct { TotalMsgs int64 `json:"total_msgs"`; SentMsgs int64 `json:"sent_msgs"`; ReceivedMsgs int64 `json:"received_msgs"`; UniqueMsgTypes int `json:"unique_msg_types"` }
	type OverviewGroup struct { ChatRoomName string `json:"ChatRoomName"`; NickName string `json:"NickName"`; MemberCount int `json:"member_count"`; MessageCount int64 `json:"message_count"` }
	type Timeline struct { Earliest int64 `json:"earliest_msg_time"`; Latest int64 `json:"latest_msg_time"`; Duration float64 `json:"duration_days"` }
	type Migration struct { ID int `json:"id"`; File string `json:"file"`; Status string `json:"status"`; CreatedAt string `json:"created_at"` }
	type Overview struct {
		User       string                    `json:"user"`
		DBStats    DBStats                   `json:"dbStats"`
		MsgStats   MsgStats                  `json:"msgStats"`
		MsgTypes   map[string]int64          `json:"msgTypes"`
		Groups     []OverviewGroup           `json:"groups"`
		Timeline   Timeline                  `json:"timeline"`
		Migrations []Migration               `json:"migrations"`
	}

	type TrendPoint struct { Date string `json:"date"`; Sent int64 `json:"sent"`; Received int64 `json:"received"` }
	type HeatmapRow struct {
		Hour     int   `json:"hour"`
		Monday   int64 `json:"monday"`
		Tuesday  int64 `json:"tuesday"`
		Wednesday int64 `json:"wednesday"`
		Thursday int64 `json:"thursday"`
		Friday   int64 `json:"friday"`
		Saturday int64 `json:"saturday"`
		Sunday   int64 `json:"sunday"`
	}

	type GroupOverview struct {
		TotalGroups int    `json:"total_groups"`
		ActiveGroups int   `json:"active_groups"`
		TodayMessages int  `json:"today_messages"`
		WeeklyAvg int      `json:"weekly_avg"`
		MostActiveHour string `json:"most_active_hour"`
	}
	type ContentAnalysis struct { Text int64 `json:"text_messages"`; Images int64 `json:"images"`; Voice int64 `json:"voice_messages"`; Files int64 `json:"files"`; Links int64 `json:"links"`; Others int64 `json:"others"` }
	type GroupListItem struct { Name string `json:"name"`; Members int `json:"members"`; Messages int64 `json:"messages"`; Active bool `json:"active"` }
	type HourlyActivity struct { Hour string `json:"hour"`; Messages int64 `json:"messages"` }
	type GroupAnalysis struct {
		Title string `json:"title"`
		Overview GroupOverview `json:"overview"`
		ContentAnalysis ContentAnalysis `json:"content_analysis"`
		GroupList []GroupListItem `json:"group_list"`
		HourlyActivity []HourlyActivity `json:"hourly_activity"`
	}
	type ContentTypeStats struct { Count int64 `json:"count"`; Percentage float64 `json:"percentage"`; SizeMB float64 `json:"size_mb"`; Trend string `json:"trend"` }
	type SourceChannel struct { Count int64 `json:"count"`; Percentage float64 `json:"percentage"`; AvgSize float64 `json:"avg_size"`; Growth string `json:"growth"` }
	type ProcessingStatus struct { Processed float64 `json:"processed"`; Processing float64 `json:"processing"`; Pending float64 `json:"pending"` }
	type QualityMetrics struct { DataIntegrity float64 `json:"data_integrity"`; ClassificationAccuracy float64 `json:"classification_accuracy"`; DuplicateRate float64 `json:"duplicate_rate"`; ErrorRate float64 `json:"error_rate"` }
	type DataTypeAnalysis struct {
		Title string `json:"title"`
		ContentTypes map[string]ContentTypeStats `json:"content_types"`
		SourceChannels map[string]SourceChannel `json:"source_channels"`
		ProcessingStatus ProcessingStatus `json:"processing_status"`
		QualityMetrics QualityMetrics `json:"quality_metrics"`
	}
	type Visualization struct {
		TrendData []TrendPoint `json:"trendData"`
		HeatmapData []HeatmapRow `json:"heatmapData"`
		GroupAnalysis GroupAnalysis `json:"groupAnalysis"`
		DataTypeAnalysis DataTypeAnalysis `json:"dataTypeAnalysis"`
	}
	type Network struct { Nodes []any `json:"nodes"`; Links []any `json:"links"` }
	type Dashboard struct { Overview Overview `json:"overview"`; Visualization Visualization `json:"visualization"`; Network Network `json:"network"` }

	ogroups := make([]OverviewGroup, 0, len(overviewGroups))
	for _, g := range overviewGroups {
		ogroups = append(ogroups, OverviewGroup{
			ChatRoomName: g["ChatRoomName"].(string),
			NickName:     g["NickName"].(string),
			MemberCount:  g["member_count"].(int),
			MessageCount: g["message_count"].(int64),
		})
	}
	tpoints := make([]TrendPoint, 0, len(trendData))
	for _, t := range trendData { tpoints = append(tpoints, TrendPoint{ Date: t["date"].(string), Sent: t["sent"].(int64), Received: t["received"].(int64) }) }
	hrows := make([]HeatmapRow, 0, 24)
	for h := 0; h < 24; h++ {
		hrows = append(hrows, HeatmapRow{
			Hour: h,
			Monday: heat[h][1],
			Tuesday: heat[h][2],
			Wednesday: heat[h][3],
			Thursday: heat[h][4],
			Friday: heat[h][5],
			Saturday: heat[h][6],
			Sunday: heat[h][0],
		})
	}
	// group list typed
	glist := make([]GroupListItem, 0, len(groupList))
	for _, it := range groupList {
		glist = append(glist, GroupListItem{
			Name: it["name"].(string),
			Members: it["members"].(int),
			Messages: it["messages"].(int64),
			Active: it["active"].(bool),
		})
	}
	// hourly activity typed
	hacts := make([]HourlyActivity, 0, len(hourlyActivity))
	for _, it := range hourlyActivity {
		hacts = append(hacts, HourlyActivity{ Hour: it["hour"].(string), Messages: it["messages"].(int64) })
	}

	// ====== 今日群聊消息数统计 ======
	todayMessages := int64(0)
	if s.db != nil && s.db.GetDB() != nil {
		if todayCounts, err := s.db.GetDB().GroupTodayMessageCounts(); err == nil {
			for _, v := range todayCounts {
				todayMessages += v
			}
		}
	}


	// ====== 本周群聊平均每天消息数 ======
	weeklyAvg := 0
	if s.db != nil && s.db.GetDB() != nil {
		if weekTotal, err := s.db.GetDB().GroupWeekMessageCount(); err == nil && weekTotal > 0 {
			// 计算已过天数：周一=1, 周二=2 ... 周六=6, 周日=7（显示完整7天平均）
			now := time.Now()
			wday := int(now.Weekday()) // Sunday=0
			passed := 0
			if wday == 0 { // Sunday
				passed = 7
			} else {
				passed = wday
			}
			if passed <= 0 { passed = 1 }
			avg := float64(weekTotal) / float64(passed)
			weeklyAvg = int(math.Round(avg))
		}
	}

	vis := Visualization{
		TrendData: tpoints,
		HeatmapData: hrows,
		GroupAnalysis: GroupAnalysis{
			Title: "群聊分析",
			Overview: GroupOverview{ TotalGroups: len(overviewGroups), ActiveGroups: activeGroups, TodayMessages: int(todayMessages), WeeklyAvg: weeklyAvg, MostActiveHour: mostActiveHour },
			// 扩展：增加 links 字段（结构体需更新）
			ContentAnalysis: ContentAnalysis{ Text: msgTypes["文本消息"], Images: msgTypes["图片消息"], Voice: msgTypes["语音消息"], Files: msgTypes["文件消息"], Links: msgTypes["链接消息"], Others: totalMsgs - (msgTypes["文本消息"]+msgTypes["图片消息"]+msgTypes["语音消息"]+msgTypes["文件消息"]+msgTypes["链接消息"]) },
			GroupList: glist,
			HourlyActivity: hacts,
		},
		DataTypeAnalysis: DataTypeAnalysis{
			Title: "数据类型统计",
			ContentTypes: map[string]ContentTypeStats{
				"文本消息":   { Count: msgTypes["文本消息"], Percentage: pct(msgTypes["文本消息"]), SizeMB: 0, Trend: "" },
				"图片消息":   { Count: msgTypes["图片消息"], Percentage: pct(msgTypes["图片消息"]), SizeMB: 0, Trend: "" },
				"语音消息":   { Count: msgTypes["语音消息"], Percentage: pct(msgTypes["语音消息"]), SizeMB: 0, Trend: "" },
				"文件消息":   { Count: msgTypes["文件消息"], Percentage: pct(msgTypes["文件消息"]), SizeMB: 0, Trend: "" },
				"链接消息":   { Count: msgTypes["链接消息"], Percentage: pct(msgTypes["链接消息"]), SizeMB: 0, Trend: "" },
				"XML消息":   { Count: msgTypes["XML消息"], Percentage: pct(msgTypes["XML消息"]), SizeMB: 0, Trend: "" },
				"好友验证消息": { Count: msgTypes["好友验证消息"], Percentage: pct(msgTypes["好友验证消息"]), SizeMB: 0, Trend: "" },
				"好友推荐消息": { Count: msgTypes["好友推荐消息"], Percentage: pct(msgTypes["好友推荐消息"]), SizeMB: 0, Trend: "" },
				"聊天表情":   { Count: msgTypes["聊天表情"], Percentage: pct(msgTypes["聊天表情"]), SizeMB: 0, Trend: "" },
				"位置消息":   { Count: msgTypes["位置消息"], Percentage: pct(msgTypes["位置消息"]), SizeMB: 0, Trend: "" },
				"音视频通话": { Count: msgTypes["音视频通话"], Percentage: pct(msgTypes["音视频通话"]), SizeMB: 0, Trend: "" },
				"手机端操作消息": { Count: msgTypes["手机端操作消息"], Percentage: pct(msgTypes["手机端操作消息"]), SizeMB: 0, Trend: "" },
				"系统通知":   { Count: msgTypes["系统通知"], Percentage: pct(msgTypes["系统通知"]), SizeMB: 0, Trend: "" },
				"撤回消息":   { Count: msgTypes["撤回消息"], Percentage: pct(msgTypes["撤回消息"]), SizeMB: 0, Trend: "" },
			},
			SourceChannels: map[string]SourceChannel{
				"私聊数据": { Count: privateTotal, Percentage: pct(privateTotal), AvgSize: 0, Growth: "" },
				"群聊数据": { Count: groupTotal,   Percentage: pct(groupTotal),   AvgSize: 0, Growth: "" },
			},
			ProcessingStatus: ProcessingStatus{ Processed: 100, Processing: 0, Pending: 0 },
			QualityMetrics: QualityMetrics{ DataIntegrity: 0, ClassificationAccuracy: 0, DuplicateRate: 0, ErrorRate: 0 },
		},
	}

	// ===== Network（亲密度）=====
	// 获取基础亲密度数据
	netNodes := make([]any, 0)
	netLinks := make([]any, 0)
	if s.db != nil && s.db.GetDB() != nil {
		if ibase, err := s.db.GetDB().IntimacyBase(); err == nil && len(ibase) > 0 {
			// 忽略的系统/服务账号
			skipIDs := map[string]struct{}{
				"filehelper":   {},
				"weixin":       {},
				"notifymessage":{},
				"fmessage":     {},
			}
			// 取联系人信息用于展示名称与头像
			contactMap := map[string]*model.Contact{}
			if clist, err := s.db.GetContacts("", 0, 0); err == nil && clist != nil {
				for _, ct := range clist.Items { if ct != nil { contactMap[ct.UserName] = ct } }
			}
			// 排序：按最近90天消息数、总消息数、过去7天发送数综合排序
			type pair struct{ k string; v *model.IntimacyBase }
			arr := make([]pair, 0, len(ibase))
			for k, v := range ibase { arr = append(arr, pair{k, v}) }
			sort.Slice(arr, func(i, j int) bool {
				ai, aj := arr[i].v, arr[j].v
				if ai.Last90DaysMsg != aj.Last90DaysMsg { return ai.Last90DaysMsg > aj.Last90DaysMsg }
				if ai.MsgCount != aj.MsgCount { return ai.MsgCount > aj.MsgCount }
				return ai.Past7DaysSentMsg > aj.Past7DaysSentMsg
			})
			// 只取前 N 个以避免图过大
			maxN := 100
			if len(arr) < maxN { maxN = len(arr) }
			// 计算有效最大分（排除自身与忽略账号）用于归一化
			effMax := 0.0
			for i := 0; i < len(arr); i++ {
				k := arr[i].k; v := arr[i].v
				if accountID != "" && k == accountID { continue }
				if _, skip := skipIDs[k]; skip { continue }
				raw := float64(v.Last90DaysMsg)*0.6 + float64(v.MsgCount)*0.3 + float64(v.Past7DaysSentMsg)*0.1
				if raw > effMax { effMax = raw }
			}
			// 节点构造
			added := 0
			for idx := 0; idx < len(arr) && added < maxN; idx++ {
				k := arr[idx].k; v := arr[idx].v
				// 过滤自身账户
				if accountID != "" && k == accountID { continue }
				if _, skip := skipIDs[k]; skip { continue }
				ct := contactMap[k]
				display := k
				remark := ""
				if ct != nil {
					if strings.TrimSpace(ct.Remark) != "" { display = ct.Remark } else if strings.TrimSpace(ct.NickName) != "" { display = ct.NickName }
					remark = ct.Remark
				}
				size := v.MsgCount
				if size < 1 { size = 1 }
				// 简单亲密度评分：最近90天权重大 + 历史总量 + 过去7天发送
				rawScore := float64(v.Last90DaysMsg)*0.6 + float64(v.MsgCount)*0.3 + float64(v.Past7DaysSentMsg)*0.1
				// 归一化（0..1）与 0..100
				norm := 0.0
				if effMax > 0 { norm = rawScore / effMax }
				if norm > 1 { norm = 1 }
				intimacy := math.Round(norm*100)
				node := map[string]any{
					"id": k,
					"name": display,
					"type": "contact",
					"size": size,
					"messages": v.MsgCount,
					"avatar": s.composeAvatarURL(k),
					"intimacy": int(intimacy),
					"wechatId": k,
					"remark": remark,
				}
				netNodes = append(netNodes, node)
				// 与当前用户连边
				strength := math.Round(norm*1000) / 1000 // 保留三位小数
				netLinks = append(netLinks, map[string]any{"source":"user", "target": k, "strength": strength})
				added++
			}
		}
	}

	resp := Dashboard{
		Overview: Overview{
			User: currentUser,
			DBStats: DBStats{ DbSizeMB: roundMB(dbSize), DirSizeMB: roundMB(dirSize) },
			MsgStats: MsgStats{ TotalMsgs: gstats.Total, SentMsgs: gstats.Sent, ReceivedMsgs: gstats.Received, UniqueMsgTypes: len(gstats.ByType) },
			MsgTypes: msgTypes,
			Groups: ogroups,
			Timeline: Timeline{ Earliest: gstats.EarliestUnix, Latest: gstats.LatestUnix, Duration: durationDays },
			Migrations: []Migration{},
		},
		Visualization: vis,
		Network: Network{ Nodes: netNodes, Links: netLinks },
	}

	if c.Query("download") == "1" {
		b, err := json.MarshalIndent(resp, "", "  ")
		if err != nil { c.JSON(http.StatusInternalServerError, gin.H{"error":"marshal failed", "detail": err.Error()}); return }
		c.Header("Content-Type", "application/json")
		c.Header("Content-Disposition", "attachment; filename=dashboard.json")
		c.Data(http.StatusOK, "application/json", b)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func roundMB(bytes int64) float64 {
	if bytes <= 0 { return 0 }
	// 1 MB = 1024*1024
	mb := float64(bytes) / (1024.0 * 1024.0)
	// round to 2 decimals
	return float64(int(mb*100+0.5)) / 100.0
}

func diffDays(a, b time.Time) int {
	if a.IsZero() || b.IsZero() { return 0 }
	if b.Before(a) { a, b = b, a }
	d := b.Sub(a).Hours() / 24.0
	if d < 0 { return 0 }
	return int(d + 0.5)
}

func formatTime(t time.Time) string {
	if t.IsZero() { return "" }
	return t.Format("2006-01-02 15:04:05")
}

// safeDirSize walks a directory and sums file sizes; returns 0 on error.
func safeDirSize(path string) int64 {
	var total int64
	if path == "" { return 0 }
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil { return nil }
		if info == nil || info.IsDir() { return nil }
		total += info.Size()
		return nil
	})
	return total
}

// estimateDBSize sums sizes of common DB files under workDir
func estimateDBSize(workDir string) int64 {
	if workDir == "" { return 0 }
	var total int64
	_ = filepath.Walk(workDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() { return nil }
		name := strings.ToLower(info.Name())
		if strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3") || strings.HasSuffix(name, ".db-wal") || strings.HasSuffix(name, ".db-shm") {
			total += info.Size()
		}
		return nil
	})
	return total
}

func (s *Service) handleChatlog(c *gin.Context) {
		q := struct {
			Time    string `form:"time"`
			Talker  string `form:"talker"`
			Sender  string `form:"sender"`
			Keyword string `form:"keyword"`
			Limit   int    `form:"limit"`
			Offset  int    `form:"offset"`
			Format  string `form:"format"`
		}{}

		if err := c.BindQuery(&q); err != nil { errors.Err(c, err); return }

		start, end, ok := util.TimeRangeOf(q.Time)
		if !ok { errors.Err(c, errors.InvalidArg("time")) }
		if q.Limit < 0 { q.Limit = 0 }
		if q.Offset < 0 { q.Offset = 0 }

		// 1. 未指定 talker: 分组输出
		if q.Talker == "" {
			sessionsResp, err := s.db.GetSessions("", 0, 0)
			if err != nil { errors.Err(c, err); return }
			type grouped struct {
				Talker string `json:"talker"`
				TalkerName string `json:"talkerName,omitempty"`
				Messages []*model.Message `json:"messages"`
			}
			groups := make([]*grouped,0)
			for _, sess := range sessionsResp.Items {
				msgs, err := s.db.GetMessages(start, end, sess.UserName, q.Sender, q.Keyword, 0, 0)
				if err != nil || len(msgs)==0 { continue }
				groups = append(groups, &grouped{Talker:sess.UserName, TalkerName:sess.NickName, Messages:msgs})
			}
			switch strings.ToLower(q.Format) {
			case "html":
				c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
				c.Writer.WriteString("<html><head><meta charset=\"utf-8\"><title>Chatlog</title><style>body{font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:1.4;}details{margin:8px 0;padding:4px 8px;border:1px solid #ddd;border-radius:4px; background:#fafafa;}summary{cursor:pointer;font-weight:600;} .msg{margin:4px 0;padding:4px 6px;border-left:3px solid #3498db;background:#fff;} .msg-row{display:flex;gap:8px;align-items:flex-start;} .avatar{width:28px;height:28px;border-radius:6px;object-fit:cover;background:#f2f2f2;border:1px solid #eee;flex:0 0 28px} .msg-content{flex:1;min-width:0} .meta{color:#666;font-size:12px;} pre{white-space:pre-wrap;word-break:break-word;margin:2px 0;} .talker{color:#2c3e50;} .sender{color:#8e44ad;} .time{color:#16a085;} .content{margin-left:4px;} a.media{color:#2c3e50;text-decoration:none;} a.media:hover{text-decoration:underline;}</style></head><body>")
				c.Writer.WriteString(fmt.Sprintf("<h2>All Messages %s ~ %s</h2>", start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05")))
				for _, g := range groups {
					title := g.Talker
					if g.TalkerName != "" { title = fmt.Sprintf("%s (%s)", g.TalkerName, g.Talker) }
						c.Writer.WriteString("<details open><summary>" + template.HTMLEscapeString(title) + fmt.Sprintf(" - %d 条消息</summary>", len(g.Messages)))
						for _, m := range g.Messages {
							m.SetContent("host", c.Request.Host)
							senderDisplay := m.Sender
							if m.IsSelf { senderDisplay = "我" }
							if m.SenderName != "" { senderDisplay = template.HTMLEscapeString(m.SenderName) + "(" + template.HTMLEscapeString(senderDisplay) + ")" } else { senderDisplay = template.HTMLEscapeString(senderDisplay) }
							aurl := template.HTMLEscapeString(s.composeAvatarURL(m.Sender) + "?size=big")
							c.Writer.WriteString("<div class=\"msg\"><div class=\"msg-row\"><img class=\"avatar\" src=\"" + aurl + "\" loading=\"lazy\" alt=\"avatar\" onerror=\"this.style.visibility='hidden'\"/><div class=\"msg-content\"><div class=\"meta\"><span class=\"sender>"+ senderDisplay +"</span><span class=\"time\">"+ m.Time.Format("2006-01-02 15:04:05") +"</span></div><pre>"+ messageHTMLPlaceholder(m) +"</pre></div></div></div>")
						}
						c.Writer.WriteString("</details>")
					}
					c.Writer.WriteString(previewHTMLSnippet)
					c.Writer.WriteString("</body></html>")
			case "json":
				c.JSON(http.StatusOK, groups)
			case "csv":
				c.Writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
				c.Writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=all_%s_%s.csv", start.Format("2006-01-02"), end.Format("2006-01-02")))
				c.Writer.Header().Set("Cache-Control", "no-cache")
				c.Writer.Header().Set("Connection", "keep-alive")
				c.Writer.Flush()
				csvWriter := csv.NewWriter(c.Writer)
				csvWriter.Write([]string{"Talker","TalkerName","Time","SenderName","Sender","Content"})
				for _, g := range groups { for _, m := range g.Messages { csvWriter.Write([]string{g.Talker, g.TalkerName, m.Time.Format("2006-01-02 15:04:05"), m.SenderName, m.Sender, m.PlainTextContent()}) } }
				csvWriter.Flush()
			default:
				c.Writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
				c.Writer.Header().Set("Cache-Control", "no-cache")
				c.Writer.Header().Set("Connection", "keep-alive")
				c.Writer.Flush()
				for _, g := range groups {
					header := g.Talker
					if g.TalkerName!="" { header = fmt.Sprintf("%s (%s)", g.TalkerName, g.Talker) }
					c.Writer.WriteString(header+"\n")
					for _, m := range g.Messages {
						sender := m.Sender
						if m.IsSelf { sender = "我" }
						if m.SenderName!="" { sender = m.SenderName + "("+sender+")" }
						c.Writer.WriteString(m.Time.Format("2006-01-02 15:04:05")+" "+sender+" "+m.PlainTextContent()+"\n")
					}
					c.Writer.WriteString("-----------------------------\n")
				}
			}
			return
		}

		// 2. 指定 talker: 单会话消息
		messages, err := s.db.GetMessages(start, end, q.Talker, q.Sender, q.Keyword, q.Limit, q.Offset)
		if err != nil { errors.Err(c, err); return }
		switch strings.ToLower(q.Format) {
		case "html":
			c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			c.Writer.WriteString("<html><head><meta charset=\"utf-8\"><title>Chatlog</title><style>body{font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:1.4;} .msg{margin:8px 0;padding:6px 8px;border-left:3px solid #3498db;background:#fafafa;} .msg-row{display:flex;gap:8px;align-items:flex-start;} .avatar{width:28px;height:28px;border-radius:6px;object-fit:cover;background:#f2f2f2;border:1px solid #eee;flex:0 0 28px} .msg-content{flex:1;min-width:0} .meta{color:#666;font-size:12px;margin-bottom:2px;} pre{white-space:pre-wrap;word-break:break-word;margin:0;} .sender{color:#8e44ad;} .time{color:#16a085;margin-left:6px;} a.media{color:#2c3e50;text-decoration:none;} a.media:hover{text-decoration:underline;}</style></head><body>")
			c.Writer.WriteString(fmt.Sprintf("<h2>Messages %s ~ %s (%s)</h2>", start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05"), template.HTMLEscapeString(q.Talker)))
			for _, m := range messages {
				m.SetContent("host", c.Request.Host)
				c.Writer.WriteString("<div class=\"msg\"><div class=\"msg-row\">")
				aurl := template.HTMLEscapeString(s.composeAvatarURL(m.Sender) + "?size=big")
				c.Writer.WriteString("<img class=\"avatar\" src=\"" + aurl + "\" loading=\"lazy\" alt=\"avatar\" onerror=\"this.style.visibility='hidden'\"/>")
				c.Writer.WriteString("<div class=\"msg-content\"><div class=\"meta\"><span class=\"sender\">")
				if m.SenderName != "" { c.Writer.WriteString(template.HTMLEscapeString(m.SenderName)+"(") }
				c.Writer.WriteString(template.HTMLEscapeString(m.Sender))
				if m.SenderName != "" { c.Writer.WriteString(")") }
				c.Writer.WriteString("</span><span class=\"time\">"+m.Time.Format("2006-01-02 15:04:05")+"</span></div><pre>")
				c.Writer.WriteString(messageHTMLPlaceholder(m))
				c.Writer.WriteString("</pre></div></div></div>")
			}
			c.Writer.WriteString(previewHTMLSnippet)
			c.Writer.WriteString("</body></html>")
		case "csv":
			c.Writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
			c.Writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s_%s_%s.csv", q.Talker, start.Format("2006-01-02"), end.Format("2006-01-02")))
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
			c.Writer.Flush()
			csvWriter := csv.NewWriter(c.Writer)
			csvWriter.Write([]string{"Time","SenderName","Sender","TalkerName","Talker","Content"})
			for _, m := range messages { csvWriter.Write(m.CSV(c.Request.Host)) }
			csvWriter.Flush()
		case "json":
			c.JSON(http.StatusOK, messages)
		default:
			c.Writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
			c.Writer.Flush()
			for _, m := range messages {
				c.Writer.WriteString(m.PlainText(strings.Contains(q.Talker, ","), util.PerfectTimeFormat(start, end), c.Request.Host)+"\n")
			}
	}
}

func (s *Service) handleContacts(c *gin.Context) {

	q := struct {
		Keyword string `form:"keyword"`
		Limit   int    `form:"limit"`
		Offset  int    `form:"offset"`
		Format  string `form:"format"`
	}{}

	if err := c.BindQuery(&q); err != nil {
		errors.Err(c, err)
		return
	}
	// 关键字去空白；空关键字表示返回全部
	q.Keyword = strings.TrimSpace(q.Keyword)

	list, err := s.db.GetContacts(q.Keyword, q.Limit, q.Offset)
	if err != nil {
		errors.Err(c, err)
		return
	}

	format := strings.ToLower(q.Format)
	switch format {
	case "html":
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		c.Writer.WriteHeader(http.StatusOK)
		c.Writer.Write([]byte(`<style>
  .contacts{font-family:Arial,Helvetica,sans-serif;font-size:14px;}
  .c-item{display:flex;align-items:center;gap:10px;border:1px solid #ddd;border-radius:6px;padding:6px 8px;margin:6px 0;background:#fff;box-shadow:0 1px 2px rgba(0,0,0,.04);} 
  .c-avatar{width:36px;height:36px;border-radius:50%;object-fit:cover;background:#f2f2f2;border:1px solid #eee}
  .c-name{font-weight:600;color:#2c3e50}
  .c-sub{color:#666;font-size:12px}
</style><div class="contacts">`))
		for _, contact := range list.Items {
			uname := template.HTMLEscapeString(contact.UserName)
			nick := template.HTMLEscapeString(contact.NickName)
			remark := template.HTMLEscapeString(contact.Remark)
			alias := template.HTMLEscapeString(contact.Alias)
			// compose avatar URL
			aurl := template.HTMLEscapeString(s.composeAvatarURL(contact.UserName))
			c.Writer.WriteString(`<div class="c-item">`)
			c.Writer.WriteString(`<img class="c-avatar" src="` + aurl + `" loading="lazy" onerror="this.style.visibility='hidden'"/>`)
			c.Writer.WriteString(`<div>`)
			c.Writer.WriteString(`<div class="c-name">` + nick + `</div>`)
			c.Writer.WriteString(`<div class="c-sub">` + uname)
			if remark != "" { c.Writer.WriteString(` · ` + remark) }
			if alias != "" { c.Writer.WriteString(` · alias:` + alias) }
			c.Writer.WriteString(`</div></div></div>`)
		}
		c.Writer.WriteString(`</div>`)
		return
	case "json":
		// fill avatar urls
		for _, item := range list.Items { item.AvatarURL = s.composeAvatarURL(item.UserName) }
		c.JSON(http.StatusOK, list)
	default:
		// csv
		if format == "csv" {
			// 浏览器访问时，会下载文件
			c.Writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
		} else {
			c.Writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Flush()
		c.Writer.WriteString("UserName,Alias,Remark,NickName,AvatarURL\n")
		for _, contact := range list.Items {
			avatarURL := s.composeAvatarURL(contact.UserName)
			c.Writer.WriteString(fmt.Sprintf("%s,%s,%s,%s,%s\n", contact.UserName, contact.Alias, contact.Remark, contact.NickName, avatarURL))
		}
		c.Writer.Flush()
	}
}

// composeAvatarURL builds a relative URL that the server can serve for any username
func (s *Service) composeAvatarURL(username string) string {
	if username == "" { return "" }
	return "/avatar/" + username
}

// handleAvatar serves avatar by username. For v3 returns redirect to remote URL; for v4 streams bytes.
func (s *Service) handleAvatar(c *gin.Context) {
	username := c.Param("username")
	size := c.Query("size") // optional: small|big
	avatar, err := s.db.GetAvatar(username, size)
	if err != nil {
		errors.Err(c, err)
		return
	}
	if avatar == nil {
		errors.Err(c, errors.ErrAvatarNotFound)
		return
	}
	if avatar.URL != "" {
		// external URL, redirect
		c.Redirect(http.StatusFound, avatar.URL)
		return
	}
	// inline bytes
	ct := avatar.ContentType
	if ct == "" { ct = "image/jpeg" }
	c.Data(http.StatusOK, ct, avatar.Data)
}

func (s *Service) handleChatRooms(c *gin.Context) {

	q := struct {
		Keyword string `form:"keyword"`
		Limit   int    `form:"limit"`
		Offset  int    `form:"offset"`
		Format  string `form:"format"`
	}{}

	if err := c.BindQuery(&q); err != nil {
		errors.Err(c, err)
		return
	}
	// 关键字去空白；空关键字表示返回全部
	q.Keyword = strings.TrimSpace(q.Keyword)

	list, err := s.db.GetChatRooms(q.Keyword, q.Limit, q.Offset)
	if err != nil {
		errors.Err(c, err)
		return
	}
	format := strings.ToLower(q.Format)
	switch format {
	case "json":
		// json
		c.JSON(http.StatusOK, list)
	default:
		// csv
		if format == "csv" {
			// 浏览器访问时，会下载文件
			c.Writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
		} else {
			c.Writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Flush()

		c.Writer.WriteString("Name,Remark,NickName,Owner,UserCount\n")
		for _, chatRoom := range list.Items {
			c.Writer.WriteString(fmt.Sprintf("%s,%s,%s,%s,%d\n", chatRoom.Name, chatRoom.Remark, chatRoom.NickName, chatRoom.Owner, len(chatRoom.Users)))
		}
		c.Writer.Flush()
	}
}

func (s *Service) handleSessions(c *gin.Context) {

	q := struct {
		Keyword string `form:"keyword"`
		Limit   int    `form:"limit"`
		Offset  int    `form:"offset"`
		Format  string `form:"format"`
	}{}

	if err := c.BindQuery(&q); err != nil {
		errors.Err(c, err)
		return
	}

	sessions, err := s.db.GetSessions(q.Keyword, q.Limit, q.Offset)
	if err != nil {
		errors.Err(c, err)
		return
	}
	format := strings.ToLower(q.Format)
	switch format {
	case "html":
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		c.Writer.WriteHeader(http.StatusOK)
		c.Writer.Write([]byte(`<style>
  .sessions-wrap{font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:1.5;}
  .session-item{border:1px solid #ddd;border-radius:6px;padding:8px 10px;margin:8px 0;background:#fff;box-shadow:0 1px 2px rgba(0,0,0,.04);} 
  .session-head{font-weight:600;color:#2c3e50;margin-bottom:4px;}
  .session-head .uname{color:#888;font-weight:400;margin-left:6px;}
  .session-time{color:#16a085;font-size:12px;margin-left:4px;}
  .session-content{margin-top:4px;white-space:pre-wrap;word-break:break-word;color:#333;}
</style><div class="sessions-wrap">`))
		for _, session := range sessions.Items {
			// 转义
			name := template.HTMLEscapeString(session.NickName)
			uname := template.HTMLEscapeString(session.UserName)
			content := template.HTMLEscapeString(session.Content)
			if len(content) > 400 { // 简单截断，避免过长
				content = content[:400] + "..."
			}
			content = strings.ReplaceAll(content, "\r", "")
			content = strings.ReplaceAll(content, "\n", "\n") // 让 pre-wrap 生效
			c.Writer.Write([]byte(`<div class="session-item"><div class="session-head">` + name + `<span class="uname">(` + uname + `)</span><span class="session-time">` + session.NTime.Format("2006-01-02 15:04:05") + `</span></div><div class="session-content">` + content + `</div></div>`))
		}
		c.Writer.Write([]byte(`</div>`))
		c.Writer.Write([]byte(previewHTMLSnippet))
		c.Writer.Flush()
		return
	case "csv":
		c.Writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Flush()

		c.Writer.WriteString("UserName,NOrder,NickName,Content,NTime\n")
		for _, session := range sessions.Items {
			c.Writer.WriteString(fmt.Sprintf("%s,%d,%s,%s,%s\n", session.UserName, session.NOrder, session.NickName, strings.ReplaceAll(session.Content, "\n", "\\n"), session.NTime))
		}
		c.Writer.Flush()
	case "json":
		// json
		c.JSON(http.StatusOK, sessions)
	default:
		c.Writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Flush()
		for _, session := range sessions.Items {
			c.Writer.WriteString(session.PlainText(120))
			c.Writer.WriteString("\n")
		}
		c.Writer.Flush()
	}
}

// handleDiary 返回最近N(24/48/72)小时内“我”发送的消息，按 talker 分组。
// GET /api/v1/diary?hours=(24|48|72)&format=(html|json|csv|text)
func (s *Service) handleDiary(c *gin.Context) {
	q := struct {
		Hours  int    `form:"hours"`
		Talker string `form:"talker"`
		Format string `form:"format"`
	}{}
	if err := c.BindQuery(&q); err != nil { errors.Err(c, err); return }
	// 默认24h，仅允许 24/48/72
	hours := q.Hours
	if hours == 0 { hours = 24 }
	if hours != 24 && hours != 48 && hours != 72 { hours = 24 }
	end := time.Now()
	start := end.Add(-time.Duration(hours) * time.Hour)

	// 获取会话（可选 talker 过滤）
	sessionsResp, err := s.db.GetSessions(q.Talker, 0, 0)
	if err != nil { errors.Err(c, err); return }

	type grouped struct {
		Talker string `json:"talker"`
		TalkerName string `json:"talkerName,omitempty"`
		Messages []*model.Message `json:"messages"`
	}
	groups := make([]*grouped,0)

	for _, sess := range sessionsResp.Items {
		msgs, err := s.db.GetMessages(start, end, sess.UserName, "", "", 0, 0)
		if err != nil || len(msgs)==0 { continue }
		hasSelf := false
		for _, m := range msgs { if m.IsSelf { hasSelf = true; break } }
		if !hasSelf { continue }
		groups = append(groups, &grouped{Talker:sess.UserName, TalkerName:sess.NickName, Messages:msgs})
	}

	format := strings.ToLower(q.Format)
	switch format {
	case "html":
		c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.Writer.WriteString(`<html><head><meta charset="utf-8"><title>Diary</title><style>body{font-family:Arial,Helvetica,sans-serif;font-size:14px;}details{margin:8px 0;padding:6px 8px;border:1px solid #ddd;border-radius:6px;background:#fafafa;}summary{cursor:pointer;font-weight:600;} .msg{margin:4px 0;padding:4px 6px;border-left:3px solid #2ecc71;background:#fff;} .msg-row{display:flex;gap:8px;align-items:flex-start;} .avatar{width:28px;height:28px;border-radius:6px;object-fit:cover;background:#f2f2f2;border:1px solid #eee;flex:0 0 28px} .msg-content{flex:1;min-width:0} .meta{color:#666;font-size:12px;margin-bottom:2px;} pre{white-space:pre-wrap;word-break:break-word;margin:0;} .sender{color:#27ae60;} .time{color:#16a085;margin-left:6px;} a.media{color:#2c3e50;text-decoration:none;} a.media:hover{text-decoration:underline;}</style></head><body>`)
		c.Writer.WriteString(fmt.Sprintf("<h2>最近%dh我参与过的会话全部消息（%s ~ %s）</h2>", hours, start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05")))
		for _, g := range groups {
			title := g.Talker
			if g.TalkerName != "" { title = fmt.Sprintf("%s (%s)", g.TalkerName, g.Talker) }
			c.Writer.WriteString("<details open><summary>" + template.HTMLEscapeString(title) + fmt.Sprintf(" - %d 条消息</summary>", len(g.Messages)))
			for _, m := range g.Messages {
				m.SetContent("host", c.Request.Host)
				senderDisplay := m.Sender
				if m.IsSelf { senderDisplay = "我" }
				if m.SenderName != "" {
					senderDisplay = template.HTMLEscapeString(m.SenderName) + "(" + template.HTMLEscapeString(senderDisplay) + ")"
				} else {
					senderDisplay = template.HTMLEscapeString(senderDisplay)
				}
				aurl := template.HTMLEscapeString(s.composeAvatarURL(m.Sender) + "?size=big")
				c.Writer.WriteString("<div class=\"msg\"><div class=\"msg-row\"><img class=\"avatar\" src=\"" + aurl + "\" loading=\"lazy\" alt=\"avatar\" onerror=\"this.style.visibility='hidden'\"/><div class=\"msg-content\"><div class=\"meta\"><span class=\"sender\">" + senderDisplay + "</span><span class=\"time\">" + m.Time.Format("2006-01-02 15:04:05") + "</span></div><pre>" + messageHTMLPlaceholder(m) + "</pre></div></div></div>")
			}
			c.Writer.WriteString("</details>")
		}
		c.Writer.WriteString(previewHTMLSnippet)
		c.Writer.WriteString("</body></html>")
	case "json":
		c.JSON(http.StatusOK, groups)
	case "csv":
		c.Writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Flush()
		writer := csv.NewWriter(c.Writer)
		writer.Write([]string{"Talker","TalkerName","Time","SenderName","Sender","Content"})
		for _, g := range groups { for _, m := range g.Messages { writer.Write([]string{m.Talker, m.TalkerName, m.Time.Format("2006-01-02 15:04:05"), m.SenderName, m.Sender, m.PlainTextContent()}) } }
		writer.Flush()
	default:
		c.Writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Flush()
		for _, g := range groups {
			if g.TalkerName!="" { c.Writer.WriteString(fmt.Sprintf("%s (%s)\n", g.TalkerName, g.Talker)) } else { c.Writer.WriteString(g.Talker+"\n") }
			for _, m := range g.Messages {
				senderDisplay := m.Sender
				if m.IsSelf { senderDisplay = "我" }
				if m.SenderName != "" {
					senderDisplay = m.SenderName + "(" + senderDisplay + ")"
				}
				c.Writer.WriteString(m.Time.Format("2006-01-02 15:04:05"))
				c.Writer.WriteString(" ")
				c.Writer.WriteString(senderDisplay)
				c.Writer.WriteString(" ")
				c.Writer.WriteString(m.PlainTextContent())
				c.Writer.WriteString("\n")
			}
			c.Writer.WriteString("-----------------------------\n")
		}
	}
}

func (s *Service) handleMedia(c *gin.Context, _type string) {
	key := strings.TrimPrefix(c.Param("key"), "/")
	if key == "" {
		errors.Err(c, errors.InvalidArg(key))
		return
	}

	keys := util.Str2List(key, ",")
	if len(keys) == 0 {
		errors.Err(c, errors.InvalidArg(key))
		return
	}

	var _err error
	for _, k := range keys {
		if strings.Contains(k, "/") {
			if absolutePath, err := s.findPath(_type, k); err == nil {
				c.Redirect(http.StatusFound, "/data/"+absolutePath)
				return
			}
		}
		media, err := s.db.GetMedia(_type, k)
		if err != nil {
			_err = err
			continue
		}
		if c.Query("info") != "" {
			c.JSON(http.StatusOK, media)
			return
		}
		switch media.Type {
		case "voice":
			s.HandleVoice(c, media.Data)
			return
		default:
			c.Redirect(http.StatusFound, "/data/"+media.Path)
			return
		}
	}

	if _err != nil {
		errors.Err(c, _err)
		return
	}
}

func (s *Service) findPath(_type string, key string) (string, error) {
	absolutePath := filepath.Join(s.conf.GetDataDir(), key)
	if _, err := os.Stat(absolutePath); err == nil {
		return key, nil
	}
	switch _type {
	case "image":
		for _, suffix := range []string{"_h.dat", ".dat", "_t.dat"} {
			if _, err := os.Stat(absolutePath + suffix); err == nil {
				return key + suffix, nil
			}
		}
	case "video":
		for _, suffix := range []string{".mp4", "_thumb.jpg"} {
			if _, err := os.Stat(absolutePath + suffix); err == nil {
				return key + suffix, nil
			}
		}
	}
	return "", errors.ErrMediaNotFound
}

func (s *Service) handleMediaData(c *gin.Context) {
	relativePath := filepath.Clean(c.Param("path"))

	absolutePath := filepath.Join(s.conf.GetDataDir(), relativePath)

	if _, err := os.Stat(absolutePath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "File not found",
		})
		return
	}

	ext := strings.ToLower(filepath.Ext(absolutePath))
	switch {
	case ext == ".dat":
		s.HandleDatFile(c, absolutePath)
	default:
		// 直接返回文件
		c.File(absolutePath)
	}

}

func (s *Service) HandleDatFile(c *gin.Context, path string) {

	b, err := os.ReadFile(path)
	if err != nil {
		errors.Err(c, err)
		return
	}
	out, ext, err := dat2img.Dat2Image(b)
	if err != nil {
		c.File(path)
		return
	}

	switch ext {
	case "jpg", "jpeg":
		c.Data(http.StatusOK, "image/jpeg", out)
	case "png":
		c.Data(http.StatusOK, "image/png", out)
	case "gif":
		c.Data(http.StatusOK, "image/gif", out)
	case "bmp":
		c.Data(http.StatusOK, "image/bmp", out)
	case "mp4":
		c.Data(http.StatusOK, "video/mp4", out)
	default:
		c.Data(http.StatusOK, "image/jpg", out)
		// c.File(path)
	}
}

func (s *Service) HandleVoice(c *gin.Context, data []byte) {
	out, err := silk.Silk2MP3(data)
	if err != nil {
		c.Data(http.StatusOK, "audio/silk", data)
		return
	}
	c.Data(http.StatusOK, "audio/mp3", out)
}

// 统一占位符：将 PlainTextContent 里形如 ![标签](url) 或 [标签](url) 的模式转成超链接形式，仅显示 [标签]。
var placeholderPattern = regexp.MustCompile(`!?\[([^\]]+)\]\((https?://[^)]+)\)`)

func messageHTMLPlaceholder(m *model.Message) string {
	content := m.PlainTextContent()
	return placeholderPattern.ReplaceAllStringFunc(content, func(s string) string {
		matches := placeholderPattern.FindStringSubmatch(s)
		if len(matches) != 3 { return template.HTMLEscapeString(s) }
		fullLabel := matches[1]
		url := matches[2]
		left := fullLabel
		rest := ""
		if p := strings.Index(fullLabel, "|") ; p >= 0 {
			left = fullLabel[:p]
			rest = fullLabel[p+1:]
		}
		className := "media"
		if left == "动画表情" || left == "GIF表情" || strings.Contains(left, "表情") {
			className = "media anim"
		}
		var anchorText string
		if left == "链接" { // 保留完整形式 [链接|标题\n更多说明]
			escapedFull := template.HTMLEscapeString(fullLabel)
			escapedFull = strings.ReplaceAll(escapedFull, "\r", "")
			escapedFull = strings.ReplaceAll(escapedFull, "\n", "<br/>")
			anchorText = "[" + escapedFull + "]"
		} else if left == "文件" && rest != "" { // 文件保留文件名
			anchorText = "[文件]" + template.HTMLEscapeString(rest)
		} else {
			anchorText = "[" + template.HTMLEscapeString(left) + "]"
		}
		return `<a class="` + className + `" href="` + template.HTMLEscapeString(url) + `" target="_blank">` + anchorText + `</a>`
	})
}
