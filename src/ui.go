package main

// `acompose ui` — a live dashboard for the running stack, served from the
// same binary. The design thesis mirrors the project's: every container is
// its own machine with its own real IP, so the IP is the hero of each card.
//
// Endpoints:
//   GET  /            the dashboard (embedded, offline-capable, no CDN)
//   GET  /api/state   project + per-service status/IP/ports (polls `container`)
//   GET  /api/logs    ?service=NAME — recent log lines
//   POST /api/action  {"service":"db","op":"stop"|"start"}

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

type svcState struct {
	Name  string     `json:"name"`
	Cname string     `json:"cname"`
	Image string     `json:"image"`
	State string     `json:"state"` // running | stopped | missing
	IP    string     `json:"ip"`
	Ports []portInfo `json:"ports"`
	Deps  []string   `json:"deps"`
}

type portInfo struct {
	Host   string `json:"host"`
	Target uint32 `json:"target"`
}

type uiState struct {
	Project  string     `json:"project"`
	Network  string     `json:"network"`
	Order    []string   `json:"order"`
	Services []svcState `json:"services"`
	Time     string     `json:"time"`
}

func collectState(p *types.Project) uiState {
	// one `container ls --all` for the whole poll; tolerant text matching so
	// we don't depend on the (still-shifting) ls JSON schema
	lsOut := ""
	if out, err := exec.Command("container", "ls", "--all").CombinedOutput(); err == nil {
		lsOut = string(out)
	}
	order := toposort(p)
	st := uiState{Project: p.Name, Network: p.Name + "-net", Order: order,
		Time: time.Now().Format("15:04:05")}
	r := runner{dry: false}
	for _, name := range order {
		svc := p.Services[name]
		cname := cnameOf(p, name)
		s := svcState{Name: name, Cname: cname, Image: svc.Image}
		if svc.Image == "" && svc.Build != nil {
			s.Image = p.Name + "-" + name + " (built)"
		}
		for d := range svc.DependsOn {
			s.Deps = append(s.Deps, d)
		}
		sort.Strings(s.Deps)
		for _, prt := range svc.Ports {
			if prt.Published != "" {
				s.Ports = append(s.Ports, portInfo{Host: prt.Published, Target: prt.Target})
			}
		}
		s.State = "missing"
		if line := lsLineFor(lsOut, cname); line != "" {
			if strings.Contains(strings.ToLower(line), "running") {
				s.State = "running"
			} else {
				s.State = "stopped"
			}
		}
		if s.State == "running" {
			s.IP = getIP(r, cname)
		}
		st.Services = append(st.Services, s)
	}
	return st
}

func cmdUI(p *types.Project, addr string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, dashboardHTML)
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(collectState(p))
	})

	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, req *http.Request) {
		name := req.URL.Query().Get("service")
		if _, ok := p.Services[name]; !ok {
			http.Error(w, "unknown service", 400)
			return
		}
		out, _ := exec.Command("container", "logs", cnameOf(p, name)).CombinedOutput()
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(lines) > 200 {
			lines = lines[len(lines)-200:]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"service": name, "lines": lines})
	})

	mux.HandleFunc("/api/action", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var body struct{ Service, Op string }
		if json.NewDecoder(req.Body).Decode(&body) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if _, ok := p.Services[body.Service]; !ok {
			http.Error(w, "unknown service", 400)
			return
		}
		if body.Op != "stop" && body.Op != "start" {
			http.Error(w, "op must be stop|start", 400)
			return
		}
		var ok bool
		var detail string
		if body.Op == "start" {
			// start must work from any state — stopped is started, missing
			// (deleted / never created) is recreated the way `up` would
			ok, detail = ensureServiceRunning(p, runner{}, body.Service, true)
		} else {
			out, err := exec.Command("container", "stop", cnameOf(p, body.Service)).CombinedOutput()
			ok, detail = err == nil, strings.TrimSpace(string(out))
			if ok {
				detail = "stopped"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "output": detail})
	})

	url := "http://" + strings.Replace(addr, "0.0.0.0", "localhost", 1)
	info("dashboard on %s%s%s  (Ctrl-C to quit)", bold, url, reset)
	_ = exec.Command("open", url).Start() // best-effort on macOS; harmless elsewhere
	if err := http.ListenAndServe(addr, mux); err != nil {
		fail("%v", err)
	}
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>acompose</title>
<style>
:root{
  --bg:#11151C; --panel:#1A2029; --panel2:#161B23; --line:#2A3340;
  --text:#C9D4E3; --dim:#6B7A8F; --addr:#59C2FF;
  --run:#7FD962; --stop:#56637A; --miss:#F26D78; --amber:#FFB454;
  --mono:ui-monospace,"SF Mono",SFMono-Regular,Menlo,Consolas,monospace;
}
*{box-sizing:border-box;margin:0;padding:0}
html{background:var(--bg)}
body{font-family:var(--mono);color:var(--text);min-height:100vh;padding:28px clamp(16px,4vw,56px)}
a{color:var(--addr);text-decoration:none}
a:hover,a:focus-visible{text-decoration:underline}
:focus-visible{outline:2px solid var(--amber);outline-offset:2px;border-radius:2px}

header{display:flex;flex-wrap:wrap;align-items:baseline;gap:14px;border-bottom:1px solid var(--line);padding-bottom:14px}
.brand{font-size:13px;letter-spacing:.28em;text-transform:uppercase;color:var(--dim)}
.project{font-size:22px;font-weight:700;letter-spacing:.02em}
.net{font-size:12px;color:var(--dim)}
.pulse{margin-left:auto;font-size:12px;color:var(--dim);display:flex;align-items:center;gap:8px}
.pulse i{width:8px;height:8px;border-radius:50%;background:var(--run);display:inline-block}
@media (prefers-reduced-motion:no-preference){.pulse i{animation:beat 2s infinite}}
@keyframes beat{0%,100%{opacity:1}50%{opacity:.25}}

#toast{position:fixed;left:50%;bottom:24px;transform:translate(-50%,12px);max-width:80vw;
  background:var(--panel);border:1px solid var(--run);border-radius:8px;padding:10px 16px;
  font-size:13px;opacity:0;pointer-events:none;transition:opacity .2s,transform .2s;z-index:99}
#toast.show{opacity:1;transform:translate(-50%,0)}
#toast.err{border-color:var(--miss);color:var(--miss)}

.chain{padding:18px 0 6px;font-size:13px;color:var(--dim);letter-spacing:.04em;overflow-x:auto;white-space:nowrap}
.chain b{color:var(--text);font-weight:600}
.chain .arrow{color:var(--line);padding:0 8px}

main{display:grid;grid-template-columns:repeat(auto-fill,minmax(290px,1fr));gap:16px;padding-top:18px}
.card{background:var(--panel);border:1px solid var(--line);border-radius:6px;padding:18px 18px 14px;display:flex;flex-direction:column;gap:10px}
.card.stopped{background:var(--panel2)}
.eyebrow{display:flex;align-items:center;gap:10px;font-size:12px;letter-spacing:.18em;text-transform:uppercase;color:var(--dim)}
.lamp{width:9px;height:9px;border-radius:50%;flex:none}
.lamp.running{background:var(--run);box-shadow:0 0 8px rgba(127,217,98,.6)}
.lamp.stopped{background:var(--stop)}
.lamp.missing{background:var(--miss)}
.ip{font-size:clamp(22px,2.4vw,28px);font-weight:700;letter-spacing:.01em;color:var(--addr);font-variant-numeric:tabular-nums;word-break:break-all;cursor:copy}
.ip.none{color:var(--stop);cursor:default;font-size:16px;font-weight:400}
.meta{font-size:12px;color:var(--dim);line-height:1.7;word-break:break-all}
.meta b{color:var(--text);font-weight:500}
.ports{display:flex;flex-wrap:wrap;gap:6px}
.port{font-size:12px;border:1px solid var(--line);border-radius:3px;padding:2px 7px;color:var(--text)}
.actions{display:flex;gap:8px;margin-top:auto;padding-top:8px;border-top:1px solid var(--line)}
button{font:inherit;font-size:12px;letter-spacing:.06em;color:var(--text);background:none;border:1px solid var(--line);border-radius:3px;padding:5px 12px;cursor:pointer}
button:hover{border-color:var(--dim)}
button.primary{border-color:var(--amber);color:var(--amber)}
button:disabled{opacity:.4;cursor:default}

#logwrap{position:fixed;inset:0;background:rgba(8,10,14,.7);display:none;align-items:stretch;justify-content:flex-end}
#logwrap.show{display:flex}
#logpanel{width:min(720px,92vw);background:var(--panel2);border-left:1px solid var(--line);display:flex;flex-direction:column}
#loghead{display:flex;align-items:center;gap:12px;padding:14px 18px;border-bottom:1px solid var(--line);font-size:13px}
#loghead b{letter-spacing:.12em;text-transform:uppercase}
#loghead button{margin-left:auto}
#logbody{flex:1;overflow:auto;padding:14px 18px;font-size:12px;line-height:1.6;white-space:pre-wrap;word-break:break-all;color:var(--text)}
.empty{grid-column:1/-1;border:1px dashed var(--line);border-radius:6px;padding:48px;text-align:center;color:var(--dim);font-size:13px;line-height:2}
.empty code{color:var(--text)}
footer{padding-top:22px;font-size:11px;color:var(--dim)}
</style>
</head>
<body>
<header>
  <span class="brand">acompose</span>
  <span class="project" id="project">—</span>
  <span class="net" id="net"></span>
  <span class="pulse"><i></i><span id="ts">connecting…</span></span>
</header>
<div class="chain" id="chain"></div>
<main id="grid"><div class="empty">connecting to the stack…</div></main>
<div id="toast" role="status" aria-live="polite"></div>
<footer>each card is its own virtual machine with its own address — click an IP to copy it</footer>

<div id="logwrap" role="dialog" aria-modal="true">
  <div id="logpanel">
    <div id="loghead"><b id="logname"></b><span class="net">last 200 lines · live</span><button onclick="closeLogs()">close</button></div>
    <div id="logbody"></div>
  </div>
</div>

<script>
let logsFor=null, busy={};
async function poll(){
  try{
    const st=await (await fetch('/api/state')).json();
    document.getElementById('project').textContent=st.project;
    document.getElementById('net').textContent='network '+st.network;
    document.getElementById('ts').textContent='updated '+st.time;
    renderChain(st); render(st);
  }catch(e){ document.getElementById('ts').textContent='connection lost — retrying'; }
}
function renderChain(st){
  const el=document.getElementById('chain');
  el.innerHTML='start order  '+st.order.map(n=>{
    const s=st.services.find(x=>x.name===n)||{};
    return '<b>'+esc(n)+'</b>'+(s.state==='running'?'':' <span style="color:var(--stop)">·off</span>');
  }).join('<span class="arrow">\u2192</span>');
}
function render(st){
  const g=document.getElementById('grid');
  if(!st.services||!st.services.length){
    g.innerHTML='<div class="empty">no services found in this project<br>run <code>acompose up</code> in the project directory, then refresh</div>';return;
  }
  g.innerHTML=st.services.map(s=>{
    const ip=s.ip?'<div class="ip" tabindex="0" title="click to copy" onclick="copy(this,\''+esc(s.ip)+'\')">'+esc(s.ip)+'</div>'
                 :'<div class="ip none">'+(s.state==='running'?'address unknown':'not running')+'</div>';
    const ports=(s.ports||[]).map(p=>'<a class="port" href="http://localhost:'+p.host+'" target="_blank" rel="noopener">localhost:'+p.host+' \u2192 '+p.target+'</a>').join('');
    const deps=(s.deps&&s.deps.length)?'<br>needs <b>'+s.deps.map(esc).join(', ')+'</b>':'';
    const op=s.state==='running'?'stop':'start';
    const dis=busy[s.name]?' disabled':'';
    return '<div class="card '+s.state+'">'
      +'<div class="eyebrow"><span class="lamp '+s.state+'"></span>'+esc(s.name)+'<span style="margin-left:auto">'+s.state+'</span></div>'
      +ip
      +'<div class="meta"><b>'+esc(s.image||'?')+'</b><br>'+esc(s.cname)+deps+'</div>'
      +(ports?'<div class="ports">'+ports+'</div>':'')
      +'<div class="actions">'
      +'<button onclick="showLogs(\''+esc(s.name)+'\')">logs</button>'
      +'<button class="primary"'+dis+' onclick="act(\''+esc(s.name)+'\',\''+op+'\')">'+op+'</button>'
      +'</div></div>';
  }).join('');
}
async function act(name,op){
  busy[name]=1; render(await (await fetch('/api/state')).json());
  try{
    const res=await(await fetch('/api/action',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({service:name,op})})).json();
    if(!res.ok) toast(name+': '+op+' failed — '+(res.output||'see the acompose ui terminal'),true);
    else toast(name+' '+(res.output||op),false);
  }catch(e){ toast(name+': '+op+' request failed',true); }
  delete busy[name]; poll();
}
let toastTimer;
function toast(msg,isErr){
  let el=document.getElementById('toast');
  el.textContent=msg;
  el.className='show'+(isErr?' err':'');
  clearTimeout(toastTimer);
  toastTimer=setTimeout(()=>{el.className='';},isErr?6000:2500);
}
async function showLogs(name){
  logsFor=name;
  document.getElementById('logname').textContent=name;
  document.getElementById('logwrap').classList.add('show');
  tailLogs();
}
async function tailLogs(){
  if(!logsFor) return;
  try{
    const d=await (await fetch('/api/logs?service='+encodeURIComponent(logsFor))).json();
    const b=document.getElementById('logbody');
    const stick = b.scrollTop+b.clientHeight >= b.scrollHeight-8;
    b.textContent=(d.lines||[]).join('\n')||'(no output yet)';
    if(stick) b.scrollTop=b.scrollHeight;
  }catch(e){}
  if(logsFor) setTimeout(tailLogs,2000);
}
function closeLogs(){logsFor=null;document.getElementById('logwrap').classList.remove('show');}
document.getElementById('logwrap').addEventListener('click',e=>{if(e.target.id==='logwrap')closeLogs();});
addEventListener('keydown',e=>{if(e.key==='Escape')closeLogs();});
function copy(el,ip){navigator.clipboard&&navigator.clipboard.writeText(ip);const t=el.textContent;el.textContent='copied';setTimeout(()=>el.textContent=t,700);}
function esc(s){return String(s).replace(/[&<>"']/g,m=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]));}
poll(); setInterval(poll,2000);
</script>
</body>
</html>`
