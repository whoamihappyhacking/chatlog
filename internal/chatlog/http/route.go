package http

import (
	"embed"
	"encoding/csv"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
<style>#preview{position:fixed;top:60px;left:40px;z-index:9999;display:none;background:#1f2329;border:1px solid #444;padding:4px 4px 8px;border-radius:8px;max-width:720px;max-height:520px;box-shadow:0 4px 16px rgba(0,0,0,0.45);color:#eee;font-size:12px;resize:both;overflow:hidden;}#preview.dragging{opacity:.85;cursor:grabbing;}#preview .pv-header{display:flex;align-items:center;justify-content:space-between;gap:6px;margin:0 2px 4px 2px;font-size:12px;user-select:none;cursor:grab;}#preview .pv-header .title{flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;color:#9ecbff;font-weight:600;}#preview button{background:#2d333b;border:1px solid #555;color:#ddd;font-size:11px;padding:2px 6px;border-radius:4px;cursor:pointer;}#preview button:hover{background:#3a424b}#preview-content{max-width:100%;max-height:470px;overflow:auto;}#preview-content img,#preview-content video{max-width:100%;max-height:470px;display:block;border-radius:4px;}#preview-content audio{width:100%;margin-top:4px;}</style>
<div id="preview"><div class="pv-header"><span class="title" id="pv-title">预览</span><button id="pv-pin" title="固定/取消固定">📌</button><button id="pv-close" title="关闭">✕</button></div><div id="preview-content"></div></div>
<script>(function(){const pv=document.getElementById('preview');const pvc=document.getElementById('preview-content');const titleEl=document.getElementById('pv-title');const pinBtn=document.getElementById('pv-pin');const closeBtn=document.getElementById('pv-close');let activeLink=null;let hideTimer=null;let pinned=false;let dragState=null;let currentType='';function esc(s){return s.replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;','\'':'&#39;'}[c]));}function build(href,text){let label=text||'';label=label.replace(/^[\[]|[\]]$/g,'');currentType='text';if(/\/image\//.test(href)){currentType='image';return '<img src="'+href+'" loading="lazy" />';}if(/\/video\//.test(href)){currentType='video';return '<video src="'+href+'" controls preload="metadata"></video>'; }if(/\/voice\//.test(href)){currentType='audio';return '<audio src="'+href+'" controls preload="metadata"></audio>'; }if(/表情/.test(label)||/\.(gif|apng|webp)(\?|$)/i.test(href)){currentType='emoji';return '<img src="'+href+'" style="max-width:100%;max-height:470px;display:block;" />';}if(/\/file\//.test(href)){currentType='file';return '<div style="word-break:break-all;line-height:1.5;">文件: '+esc(label)+'<br/><a href="'+href+'" target="_blank" style="color:#61afef;">下载</a></div>'; }return '<div style="word-break:break-all;line-height:1.5;">'+esc(label)+'<br/><a href="'+href+'" target="_blank" style="color:#61afef;">打开</a></div>'; }function adjustWidth(){if(dragState)return;const vw=window.innerWidth;const clamp=w=>Math.min(w,vw-40);switch(currentType){case'audio':pv.style.width=clamp(680)+'px';break;case'video':pv.style.width=clamp(720)+'px';break;case'file':pv.style.width=clamp(560)+'px';break;case'image':case'emoji':pv.style.width='auto';break;default:pv.style.width='420px';}}function showFor(a){clearTimeout(hideTimer);activeLink=a;const href=a.getAttribute('href');pvc.innerHTML=build(href,a.textContent||'');titleEl.textContent=a.textContent||'预览';pv.style.display='block';adjustWidth();positionNear(a);}function positionNear(a){if(pinned||dragState)return;const rect=a.getBoundingClientRect();const pw=pv.offsetWidth;const ph=pv.offsetHeight;let x=rect.right+12;let y=rect.top;const vw=window.innerWidth;const vh=window.innerHeight;if(x+pw>vw-8)x=rect.left-pw-12;if(x<8)x=8;if(y+ph>vh-8)y=vh-ph-8;if(y<8)y=8;pv.style.left=x+'px';pv.style.top=y+'px';}function scheduleHide(){if(pinned)return;hideTimer=setTimeout(()=>{if(pinned)return;activeLink=null;pv.style.display='none';pvc.innerHTML='';},280);}document.addEventListener('mouseover',e=>{const a=e.target.closest('a.media');if(!a)return;if(a===activeLink){clearTimeout(hideTimer);return;}showFor(a);});document.addEventListener('mousemove',e=>{if(!activeLink||pinned||dragState)return;positionNear(activeLink);});pv.addEventListener('mouseenter',()=>{clearTimeout(hideTimer);});pv.addEventListener('mouseleave',()=>{scheduleHide();});document.addEventListener('mouseout',e=>{const a=e.target.closest&&e.target.closest('a.media');if(!a)return;if(pv.contains(e.relatedTarget))return;scheduleHide();});pinBtn.addEventListener('click',()=>{pinned=!pinned;pinBtn.style.opacity=pinned?1:0.6;if(!pinned){scheduleHide();}else{clearTimeout(hideTimer);}});closeBtn.addEventListener('click',()=>{pinned=false;activeLink=null;pv.style.display='none';pvc.innerHTML='';});pv.querySelector('.pv-header').addEventListener('mousedown',e=>{if(e.target===pinBtn||e.target===closeBtn)return;pinned=true;pinBtn.style.opacity=1;dragState={ox:e.clientX,oy:e.clientY,left:pv.offsetLeft,top:pv.offsetTop};pv.classList.add('dragging');e.preventDefault();});window.addEventListener('mousemove',e=>{if(!dragState)return;const dx=e.clientX-dragState.ox;const dy=e.clientY-dragState.oy;let nl=dragState.left+dx;let nt=dragState.top+dy;const vw=window.innerWidth;const vh=window.innerHeight;nl=Math.max(0,Math.min(vw-pv.offsetWidth,nl));nt=Math.max(0,Math.min(vh-pv.offsetHeight,nt));pv.style.left=nl+'px';pv.style.top=nt+'px';});window.addEventListener('mouseup',()=>{if(dragState){dragState=null;pv.classList.remove('dragging');}});window.addEventListener('keydown',e=>{if(e.key==='Escape'){pinned=false;pv.style.display='none';pvc.innerHTML='';activeLink=null;}});})();</script>`

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
}

func (s *Service) initAPIRouter() {
	api := s.router.Group("/api/v1", s.checkDBStateMiddleware())
	{
		api.GET("/chatlog", s.handleChatlog)
		api.GET("/contact", s.handleContacts)
		api.GET("/chatroom", s.handleChatRooms)
		api.GET("/session", s.handleSessions)
		api.GET("/diary", s.handleDiary)
	}
}

func (s *Service) initMCPRouter() {
	s.router.Any("/mcp", func(c *gin.Context) { s.mcpStreamableServer.ServeHTTP(c.Writer, c.Request) })
	s.router.Any("/sse", func(c *gin.Context) { s.mcpSSEServer.ServeHTTP(c.Writer, c.Request) })
	s.router.Any("/message", func(c *gin.Context) { s.mcpSSEServer.ServeHTTP(c.Writer, c.Request) })
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
				c.Writer.WriteString("<html><head><meta charset=\"utf-8\"><title>Chatlog</title><style>body{font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:1.4;}details{margin:8px 0;padding:4px 8px;border:1px solid #ddd;border-radius:4px; background:#fafafa;}summary{cursor:pointer;font-weight:600;} .msg{margin:4px 0;padding:4px 6px;border-left:3px solid #3498db;background:#fff;} .meta{color:#666;font-size:12px;} pre{white-space:pre-wrap;word-break:break-word;margin:2px 0;} .talker{color:#2c3e50;} .sender{color:#8e44ad;} .time{color:#16a085;} .content{margin-left:4px;} a.media{color:#2c3e50;text-decoration:none;} a.media:hover{text-decoration:underline;}</style></head><body>")
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
							c.Writer.WriteString("<div class=\"msg\"><div class=\"meta\"><span class=\"sender\">"+ senderDisplay +"</span><span class=\"time\">"+ m.Time.Format("2006-01-02 15:04:05") +"</span></div><pre>"+ messageHTMLPlaceholder(m) +"</pre></div>")
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
			c.Writer.WriteString("<html><head><meta charset=\"utf-8\"><title>Chatlog</title><style>body{font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:1.4;} .msg{margin:8px 0;padding:6px 8px;border-left:3px solid #3498db;background:#fafafa;} .meta{color:#666;font-size:12px;margin-bottom:2px;} pre{white-space:pre-wrap;word-break:break-word;margin:0;} .sender{color:#8e44ad;} .time{color:#16a085;margin-left:6px;} a.media{color:#2c3e50;text-decoration:none;} a.media:hover{text-decoration:underline;}</style></head><body>")
			c.Writer.WriteString(fmt.Sprintf("<h2>Messages %s ~ %s (%s)</h2>", start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05"), template.HTMLEscapeString(q.Talker)))
			for _, m := range messages {
				m.SetContent("host", c.Request.Host)
				c.Writer.WriteString("<div class=\"msg\"><div class=\"meta\"><span class=\"sender\">")
				if m.SenderName != "" { c.Writer.WriteString(template.HTMLEscapeString(m.SenderName)+"(") }
				c.Writer.WriteString(template.HTMLEscapeString(m.Sender))
				if m.SenderName != "" { c.Writer.WriteString(")") }
				c.Writer.WriteString("</span><span class=\"time\">"+m.Time.Format("2006-01-02 15:04:05")+"</span></div><pre>")
				c.Writer.WriteString(messageHTMLPlaceholder(m))
				c.Writer.WriteString("</pre></div>")
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

	list, err := s.db.GetContacts(q.Keyword, q.Limit, q.Offset)
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

		c.Writer.WriteString("UserName,Alias,Remark,NickName\n")
		for _, contact := range list.Items {
			c.Writer.WriteString(fmt.Sprintf("%s,%s,%s,%s\n", contact.UserName, contact.Alias, contact.Remark, contact.NickName))
		}
		c.Writer.Flush()
	}
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

// handleDiary 返回最近24小时内“我”发送的消息，按 talker 分组。
// GET /api/v1/diary?format=(html|json|csv|text)
func (s *Service) handleDiary(c *gin.Context) {
	q := struct { Format string `form:"format"` }{}
	if err := c.BindQuery(&q); err != nil { errors.Err(c, err); return }
	end := time.Now()
	start := end.Add(-24 * time.Hour)

	// 获取所有会话
	sessionsResp, err := s.db.GetSessions("", 0, 0)
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
		c.Writer.WriteString(`<html><head><meta charset="utf-8"><title>Diary - 24h</title><style>body{font-family:Arial,Helvetica,sans-serif;font-size:14px;}details{margin:8px 0;padding:6px 8px;border:1px solid #ddd;border-radius:6px;background:#fafafa;}summary{cursor:pointer;font-weight:600;} .msg{margin:4px 0;padding:4px 6px;border-left:3px solid #2ecc71;background:#fff;} .meta{color:#666;font-size:12px;margin-bottom:2px;} pre{white-space:pre-wrap;word-break:break-word;margin:0;} .sender{color:#27ae60;} .time{color:#16a085;margin-left:6px;} a.media{color:#2c3e50;text-decoration:none;} a.media:hover{text-decoration:underline;}</style></head><body>`)
		c.Writer.WriteString(fmt.Sprintf("<h2>最近24小时我参与过的会话全部消息（%s ~ %s）</h2>", start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05")))
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
				c.Writer.WriteString("<div class=\"msg\"><div class=\"meta\"><span class=\"sender\">" + senderDisplay + "</span><span class=\"time\">" + m.Time.Format("2006-01-02 15:04:05") + "</span></div><pre>" + messageHTMLPlaceholder(m) + "</pre></div>")
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
