'use strict';
const $ = id => document.getElementById(id);
const node = (tag, text, className) => { const n=document.createElement(tag); if(text!==undefined)n.textContent=text; if(className)n.className=className; return n; };
let invocationID='', selected='', generation=0, activeView='suites', catalog=[], currentRun='', types=[], objectName='', eventAfter='0', eventRows=[], eventLost=false, displayClipped=false, runAfter='', runIDs=new Set(), chosenRun='';
let writeEnabled=false, editor=null, detailSnapshot=null, editorSerial=0;
const jobs=new Map();
const selectionVersions=new Map(), pendingSelections=new Map(), selectionTimers=new Map();
const state={online:false,last:'',retry:2000};
function message(id,text){$(id).textContent=text;}
function stamp(){state.online=true;state.retry=2000;state.last=new Date().toISOString();message('connection',writeEnabled?'Connected · interactive writes enabled':'Connected · read only');message('freshness','Last observed '+state.last);}
function offline(error){if(error.name==='AbortError')return;state.online=false;state.retry=Math.min(30000,state.retry*2);message('connection','Disconnected / unavailable');message('freshness','Last observed '+(state.last||'never')+'. Freshness is not guaranteed.');}
async function request(view,path,apply,manual=false,completed=null){
 if(document.hidden)return; let job=jobs.get(view);const now=performance.now();if(job?.busy||job&&now-job.at<2000)return;
 const token=generation,selectionVersion=selectionVersions.get(view)||0;const controller=new AbortController();job={busy:true,at:now,controller};jobs.set(view,job);
 let timedOut=false;const timeout=setTimeout(()=>{timedOut=true;controller.abort();},3000);
 try{const response=await fetch('/ui/api/v1/'+path,{signal:controller.signal,cache:'no-store'});const data=await response.json();if((token!==generation&&view!=='catalog'&&view!=='report')||selectionVersion!==(selectionVersions.get(view)||0))return;
  if(!response.ok){const code=data.error?.code||'request_failed';if(response.status===429){job.at=performance.now()+Math.max(1000,Number(response.headers.get('Retry-After')||1)*1000)-2000;}const error=new Error('workbench_response');error.workbenchCode=typeof code==='string'&&/^[a-z_]{1,64}$/.test(code)?code:'request_failed';error.status=response.status;throw error;}
  if(data.sessionId&&data.sessionId!==selected)return;stamp();apply(data);
 }catch(error){
  if((token!==generation&&view!=='catalog'&&view!=='report')||selectionVersion!==(selectionVersions.get(view)||0))return;
  if(error.name==='AbortError'&&!timedOut)return;
  const code=timedOut?'request_timeout':error.workbenchCode||(error.name==='SyntaxError'?'response_invalid':'network_unavailable');
  offline(new Error(code));
  const target={catalog:'coordinator',resources:'resource-status',types:'resource-status',detail:'object-json',events:'event-status',runs:'runs-status',report:'report-status'}[view];
  if(target)message(target,error.status===410?'Session ended. Select a live session to inspect resources.':error.status===409?'Session API is not available yet.':'Unavailable: '+code);
 }
 finally{clearTimeout(timeout);job.busy=false;if(completed)completed();}
}
// Detail/report navigation has one latest intent, not a dropped click. The
// pending entry holds only a bounded URL/callback; response bodies never queue.
function drainSelection(view){
 const timer=selectionTimers.get(view);if(timer){clearTimeout(timer);selectionTimers.delete(view);}
 const intent=pendingSelections.get(view);if(!intent)return;
 if(view!=='report'&&intent.generation!==generation){pendingSelections.delete(view);return;}
 if(view==='report'&&activeView!=='suites')return;
 if(document.hidden)return;
 const job=jobs.get(view);if(job?.busy)return; // its completion resumes the latest intent
 const wait=job?Math.max(0,2000-(performance.now()-job.at)):0;
 if(wait){selectionTimers.set(view,setTimeout(()=>drainSelection(view),wait));return;}
 const startedGeneration=generation;
 // Report intents survive tab changes; bind completion to the generation in
 // which the queued request actually starts, preserving hidden-tab retries.
 if(view==='report')intent.generation=startedGeneration;
 request(view,intent.path,data=>{
  if(pendingSelections.get(view)!==intent)return;
  pendingSelections.delete(view);intent.apply(data);
 },false,()=>{
  // A hidden-tab cancellation preserves an unfinished explicit selection. An
  // ordinary completed error is shown once; it does not become detail polling.
  if(pendingSelections.get(view)===intent&&intent.generation===startedGeneration)pendingSelections.delete(view);
  drainSelection(view);
 });
}
function selectRequest(view,path,apply){
 selectionVersions.set(view,(selectionVersions.get(view)||0)+1);
 jobs.get(view)?.controller.abort();
 pendingSelections.set(view,{path,apply,generation});
 drainSelection(view);
}
function invalidate(preserveSelections=false){
 generation++;for(const [key,job] of jobs){if(key!=='catalog'&&key!=='report')job.controller.abort();}
 for(const [key,timer] of selectionTimers){if(key!=='report'||preserveSelections){clearTimeout(timer);selectionTimers.delete(key);}}
 if(preserveSelections){for(const intent of pendingSelections.values())intent.generation=generation;}
 else {for(const key of pendingSelections.keys())if(key!=='report')pendingSelections.delete(key);}
}
function descriptor(){return catalog.find(s=>s.id===selected);}
function live(){const d=descriptor();return d&&d.apiAvailable&&!['stopping','stopped','failed'].includes(d.state);}
function selectedState(){refreshWriteControls();const d=descriptor();message('session-heading',d?(d.scenario||'Operator session'):'Select a session');message('selected-id',selected);message('session-state',d?'Session '+d.state+' · Operator '+d.operatorState+' · API '+(d.apiAvailable?'available':'unavailable'):'Resources are scoped to one session.');if(d&&['stopped','stopping','failed'].includes(d.state)){$('objects').replaceChildren();$('detail').hidden=true;objectName='';}if(d&&!live())message('resource-status',d.operatorState==='notConfigured'?'No operator/API configured. Browse saved results.':['stopped','stopping','failed'].includes(d.state)?'Session ended. Historical resource snapshots are unavailable. Retained events remain available.':'Waiting for the session API.');}
function selectSession(id){if(id===selected)return;if(!leaveEditor()){$('session-select').value=selected;return;}detailSnapshot=null;invalidate();selected=id;types=[];objectName='';eventAfter='0';eventRows=[];eventLost=false;displayClipped=false;$('objects').replaceChildren();$('event-list').replaceChildren();$('types').replaceChildren(node('option','Loading resource types…'));$('detail').hidden=true;$('event-gap').hidden=true;message('event-status','No event page loaded.');selectedState();drawSessions();loadTypes();if(activeView==='events')loadEvents(true);}
function drawSessions(){const signature=JSON.stringify(catalog);if(signature!==sessionSignature){sessionSignature=signature;const empty=node('option','Select a session');empty.value='';$('session-select').replaceChildren(empty,...catalog.map(s=>{const option=node('option',(s.scenario||'Operator session')+' · '+s.id+' · '+s.state+' · operator '+s.operatorState);option.value=s.id;return option;}));}$('session-select').value=selected;}
function loadCatalog(){request('catalog','sessions',data=>{if(invocationID&&invocationID!==data.invocationId){invalidate();cancelReport();selectSession('');runIDs.clear();runAfter='';$('run-list').replaceChildren();}invocationID=data.invocationId;catalog=data.sessions;writeEnabled=data.writeEnabled===true;message('access-mode',writeEnabled?'Interactive writes enabled':'Read only');currentRun=data.currentRunId;message('invocation',data.invocationId);message('coordinator','Run coordinator reachable. Session and operator readiness are shown separately.');drawSessions();selectedState();renderSessionLinks();$('current-run').hidden=!currentRun;if(selected&&live()&&types.length===0)loadTypes();});}
function selectedType(){return types[Number($('types').value)];}
function query(name=''){const t=selectedType();if(!t)return '';const q=new URLSearchParams({apiVersion:t.apiVersion,kind:t.kind});if(t.scope==='Namespaced')q.set('namespace',$('namespace').value);if(name)q.set('name',name);return 'sessions/'+selected+'/objects?'+q.toString();}
function loadTypes(){if(!selected||!live())return;request('types','sessions/'+selected+'/types',data=>{types=data.types;$('types').replaceChildren(...types.map((t,i)=>{const o=node('option',t.kind+' · '+t.apiVersion);o.value=String(i);return o;}));typeChanged();});}
function typeChanged(){if(!leaveEditor()){if(editor){$('types').value=String(editor.typeIndex);$('namespace').value=editor.namespace;}return;}detailSnapshot=null;invalidate();objectName='';$('objects').replaceChildren();$('detail').hidden=true;$('namespace').disabled=selectedType()?.scope!=='Namespaced';refreshWriteControls();loadObjects();}
function loadObjects(){if(!live()||!selectedType())return;message('resource-status','Loading current snapshot…');request('resources',query(),data=>{message('resource-status',data.items.length+' object(s) · '+new Date().toLocaleTimeString());$('objects').replaceChildren(...data.items.map(item=>{const row=node('tr');const cell=node('td');const b=node('button',item.name);b.onclick=()=>loadDetail(item.name);cell.append(b);row.append(cell,node('td',item.namespace||'—'),node('td',item.uid),node('td',item.resourceVersion));return row;}));});}
function loadDetail(name){if(!live())return;if(editor&&(editor.mode==='create'||name!==editor.name)&&!leaveEditor())return;detailSnapshot=null;refreshWriteControls();objectName=name;$('detail').hidden=false;message('detail-name',name);message('object-json','Loading…');selectRequest('detail',query(name),data=>{message('object-json',data.objectJson);detailSnapshot={raw:data.objectJson,uid:data.uid,rv:data.resourceVersion,name,session:selected,type:selectedType(),namespace:selectedType()?.scope==='Namespaced'?$('namespace').value:''};refreshWriteControls();});}
function loadEvents(manual=false){if(!selected||!manual&&!live())return;request('events','sessions/'+selected+'/events?after='+eventAfter+'&limit=100',data=>{eventAfter=data.nextAfter;eventRows.push(...data.events);let clipped=eventRows.length>500;if(clipped)eventRows=eventRows.slice(-500);eventLost=eventLost||data.gap;displayClipped=displayClipped||clipped;if(eventLost||displayClipped){$('event-gap').hidden=false;message('event-gap',(eventLost?'Retained history has gaps. Omitted events cannot be reconstructed here. ':'')+(displayClipped?'Older displayed rows truncated; at most 500 rows are shown.':''));}message('event-status','Observed through '+data.observedLast+' · next cursor '+eventAfter+' · '+eventRows.length+' displayed');$('event-list').replaceChildren(...eventRows.map(event=>{const li=node('li');li.append(node('strong',event.sequence+' · '+event.kind),node('p',(event.timestamp||'')+' · Actor: '+(event.actor||'unknown')+' · Correlation: '+(event.correlationId||'none')),node('pre',JSON.stringify(event.payload||{},null,2)));return li;}));},manual);}
const outcomes=['passed','failed','error','cancelled','skipped','pending','running'];
const marks={passed:'✓',failed:'×',error:'!',cancelled:'–',skipped:'↷',pending:'○',running:'◷',notStarted:'○'};
let review=null, groups=[], selectedScenario='', collapsed=new Set(), groupButtons=new Map(), sessionSignature='', linkSignature='';
function statusNode(value){const n=node('span',(marks[value]||'·')+' '+(value==='notStarted'?'Not started':value),'status');if(outcomes.includes(value))n.classList.add('status-'+value);return n;}
function recorded(value){return value===undefined||value===null||value===''?'Not recorded':String(value);}
function humanDuration(value){
 if(value===undefined||value===null)return 'Not recorded';
 const n=BigInt(value);let unit=1n,suffix='ns',digits=0;
 if(n>=1000000000n){unit=1000000000n;suffix='s';digits=9;}else if(n>=1000000n){unit=1000000n;suffix='ms';digits=6;}
 const fraction=digits?(n%unit).toString().padStart(digits,'0').replace(/0+$/,''):'';
 return (n/unit).toString()+(fraction?'.'+fraction:'')+' '+suffix;
}
function durationNode(value){const n=node('details',undefined,'duration');const summary=node('summary',humanDuration(value));summary.setAttribute('aria-label',humanDuration(value)+'; expand for exact nanoseconds');n.append(summary,node('code',recorded(value)+(value===undefined?'':' ns')));return n;}
function field(dl,label,value){dl.append(node('dt',label),node('dd',recorded(value)));}
function clearInspector(announce=false){selectedScenario='';renderInspector();if(announce)message('selection-announcement','Scenario selection cleared because it is no longer visible.');}
function resetReview(){
 review=null;groups=[];groupButtons.clear();selectedScenario='';collapsed.clear();$('scenario-search').value='';$('scenario-outcome').value='all';
 $('run-summary').replaceChildren();$('run-counts').replaceChildren();$('scenario-rows').replaceChildren();$('report-json').textContent='';$('report-raw').hidden=true;$('review-content').hidden=true;$('review-empty').hidden=false;$('report-refresh').disabled=true;message('report-observed','');renderInspector();
}
function cancelReport(){selectionVersions.set('report',(selectionVersions.get('report')||0)+1);jobs.get('report')?.controller.abort();pendingSelections.delete('report');clearTimeout(selectionTimers.get('report'));selectionTimers.delete('report');chosenRun='';resetReview();}
function makeGroups(){
 const map=new Map();groups=[];
 for(const scenario of review.scenarios){const chain=scenario.suitePaths||[],key=JSON.stringify(chain);let group=map.get(key);if(!group){group={key,chain,scenarios:[],counts:{}};map.set(key,group);groups.push(group);}group.scenarios.push(scenario);group.counts[scenario.status]=(group.counts[scenario.status]||0)+1;}
 collapsed=new Set([...collapsed].filter(key=>map.has(key)));
}
function matches(s){const query=$('scenario-search').value.toLowerCase(),outcome=$('scenario-outcome').value;return (outcome==='all'||s.status===outcome)&&(!query||[s.name,s.sourcePath,...(s.suitePaths||[])].some(v=>v.toLowerCase().includes(query)));}
function countNodes(counts){return outcomes.filter(k=>BigInt(counts[k]||'0')>0n).map(k=>{const item=node('span',undefined);item.append(statusNode(k),node('span',' '+counts[k]));return item;});}
function renderLedger(){
 if(!review)return;
 const rows=[],visible=new Set();let count=0;groupButtons.clear();
 for(const group of groups){
  const matching=group.scenarios.filter(matches);count+=matching.length;if(!matching.length)continue;
  const row=node('tr',undefined,'group-row'),cell=node('td');cell.colSpan=4;
  const shut=collapsed.has(group.key),label=group.chain.length?group.chain[group.chain.length-1]:'Ungrouped scenarios';
  const button=node('button',(shut?'▸ ':'▾ ')+label,'group-toggle');button.setAttribute('aria-expanded',String(!shut));button.onclick=()=>{if(collapsed.has(group.key))collapsed.delete(group.key);else collapsed.add(group.key);renderLedger();groupButtons.get(group.key)?.focus();};groupButtons.set(group.key,button);
  const counts=node('div',undefined,'group-counts');counts.append(...countNodes(group.counts),node('span',group.scenarios.length+' total'));
  cell.append(button,node('span',group.chain.length?group.chain.join(' → '):'No suite chain recorded','group-chain'),counts);row.append(cell);rows.push(row);
  if(shut)continue;
  for(const scenario of matching){
   visible.add(scenario.sourcePath);const row=node('tr',undefined,'scenario-row');row.dataset.source=scenario.sourcePath;row.setAttribute('aria-selected',String(selectedScenario===scenario.sourcePath));
   const name=node('td'),select=node('button',scenario.name,'scenario-select');select.setAttribute('aria-pressed',String(selectedScenario===scenario.sourcePath));select.setAttribute('aria-label',scenario.name+' · '+scenario.status+' · '+scenario.sourcePath);select.onclick=()=>selectScenario(scenario.sourcePath);name.append(select);
   const outcome=node('td');outcome.append(statusNode(scenario.status));const time=node('td');time.append(durationNode(scenario.durationNs));row.append(name,outcome,time,node('td',scenario.sourcePath,'source'));rows.push(row);
  }
 }
 $('scenario-rows').replaceChildren(...rows);message('scenario-count',count+' of '+review.scenarios.length+' scenarios shown · matching count excludes collapse state');$('no-matches').hidden=count!==0;$('scenario-table').hidden=count===0;
 if(selectedScenario&&!visible.has(selectedScenario))clearInspector(true);
}
function selectScenario(source){
 if(!review)return;selectedScenario=source;
 for(const row of $('scenario-rows').querySelectorAll('.scenario-row')){const on=row.dataset.source===source;row.setAttribute('aria-selected',String(on));row.querySelector('.scenario-select').setAttribute('aria-pressed',String(on));}
 message('selection-announcement','Selected '+review.scenarios.find(s=>s.sourcePath===source).name);renderInspector();
}
function renderSessionLinks(){
 const scenario=review?.scenarios.find(s=>s.sourcePath===selectedScenario),id=scenario?.sessionId,entry=catalog.find(s=>s.id===id);
 const signature=JSON.stringify([chosenRun,selectedScenario,id,!!entry]);if(signature===linkSignature)return;linkSignature=signature;
 const box=$('inspector-links');box.replaceChildren();if(!scenario)return;
 if(!id){box.append(node('p','Session: Not recorded','muted'));return;}
 if(!entry){box.append(node('p','Session inspection is unavailable in this invocation. Historical resource snapshots are not retained.','muted'));return;}
 box.className='inspector-links';for(const target of ['resources','events']){const b=node('button','View session '+target);b.onclick=()=>{selectSession(id);view(target);};box.append(b);}
}
function renderInspector(){
 const pane=$('report'),scenario=review?.scenarios.find(s=>s.sourcePath===selectedScenario);pane.replaceChildren();linkSignature='';$('jump-details').disabled=!scenario;
 if(!scenario){pane.append(node('p','Select a scenario to inspect its recorded steps and cleanup.','muted'));renderSessionLinks();return;}
 pane.append(node('h3',scenario.name));const outcome=node('p',undefined,['failed','error'].includes(scenario.status)?'failure-note':'');outcome.append(node('span','Recorded scenario '),statusNode(scenario.status));pane.append(outcome);
 const facts=node('dl');field(facts,'Source',scenario.sourcePath);field(facts,'Suite chain',(scenario.suitePaths||[]).length?scenario.suitePaths.join(' → '):'Ungrouped scenarios');field(facts,'Reason code',scenario.reasonCode);field(facts,'Duration',humanDuration(scenario.durationNs)+' · '+scenario.durationNs+' ns');field(facts,'Recorded session ID',scenario.sessionId);field(facts,'Recorded start',scenario.startedAt);field(facts,'Recorded end',scenario.endedAt);pane.append(facts);
 if(scenario.steps.some(step=>step.operation==='assert'))pane.append(node('p','Assertion values were not recorded in this report','notice'));
 pane.append(node('h3','Recorded steps'));
 for(const step of scenario.steps){const box=node('section',undefined,'step'),title=node('div',undefined,'step-title');title.append(node('strong',step.id+' · '+step.operation),statusNode(step.outcome));box.append(title);const data=node('dl');field(data,'Duration',humanDuration(step.durationNs)+' · '+step.durationNs+' ns');field(data,'Reason code',step.reasonCode);
  const ids=step.requestIds||[];
  if(step.operation==='assert'){field(data,'Sample count',step.sampleCount);if(ids.length>1){field(data,'First retained request ID',ids[0]);field(data,'Latest retained request ID',ids[ids.length-1]);}else field(data,'First/latest retained request ID',ids[0]);box.append(node('p','Retained IDs do not enumerate every sample or prove assertion success.','muted'));}
  else {field(data,'Recorded request IDs',ids.length?ids.join(' · '):undefined);if(step.sampleCount!==undefined)field(data,'Recorded sample count',step.sampleCount);}
  box.append(data);pane.append(box);
 }
 const cleanup=node('section',undefined,'cleanup');cleanup.append(node('h3','Cleanup'),statusNode(scenario.cleanup.outcome));const data=node('dl');field(data,'Reason code',scenario.cleanup.reasonCode);field(data,'Duration',humanDuration(scenario.cleanup.durationNs)+' · '+scenario.cleanup.durationNs+' ns');cleanup.append(data);pane.append(cleanup);renderSessionLinks();
}
function showReview(data,id){
 if(id!==chosenRun)return;review=data.reportView;makeGroups();$('review-empty').hidden=true;$('review-content').hidden=false;
 const summary=$('run-summary');summary.replaceChildren(node('h2','Recorded run '+review.runId));const metadata=node('div',undefined,'metadata');metadata.append(node('span','Selection: '+review.selectionPath),node('span','Started: '+review.startedAt),node('span','Duration: '+humanDuration(review.durationNs)+' · '+review.durationNs+' ns'),node('span','Recorded exit code: '+recorded(review.exitCode)),node('span','Run reason: '+recorded(review.reasonCode)));summary.append(statusNode(review.status),metadata);
 $('run-counts').replaceChildren(node('strong',review.counts.total+' scenarios'),...countNodes(review.counts));renderLedger();renderInspector();
 message('report-status',review.status==='running'?'Recorded running report: incomplete; this does not prove its process is currently alive.':'Saved outcome; process exit is not inferred.');message('report-observed','Report observed '+new Date().toISOString()+' · refresh is manual.');$('report-raw').hidden=false;message('report-json',data.reportJson);
}
function loadRuns(){request('runs','runs?after='+runAfter+'&limit=50',data=>{runAfter=data.nextAfter;for(const id of data.ids){if(runIDs.size>=10000)break;runIDs.add(id);}message('runs-status',runIDs.size?'Select a saved run. IDs are listed without inferred status or date.':'No saved reports. Operator startup records sessions; use --tests to create a test report.');$('run-list').replaceChildren(...[...runIDs].map(id=>{const b=node('button',id,'run');b.onclick=()=>loadReport(id);return b;}));});}
function loadReport(id){
 if(!leaveEditor())return;
 if(id!==chosenRun){resetReview();chosenRun=id;}
 review=null;groups=[];groupButtons.clear();$('scenario-rows').replaceChildren();$('run-summary').replaceChildren();$('run-counts').replaceChildren();$('report-json').textContent='';$('report-raw').hidden=true;$('review-content').hidden=true;$('review-empty').hidden=true;renderInspector();$('report-refresh').disabled=false;message('report-observed','');message('report-status','Loading saved report '+id+'…');
 selectRequest('report','runs/'+id,data=>showReview(data,id));view('suites');
}
function view(name){if(name!==activeView&&!leaveEditor())return;invalidate();activeView=name;for(const section of document.querySelectorAll('.view'))section.hidden=section.id!==name;for(const b of document.querySelectorAll('[data-view]'))b.setAttribute('aria-pressed',String(b.dataset.view===name));$('session-tools').hidden=!['resources','events'].includes(name);if(name==='resources'){if(types.length)loadObjects();else loadTypes();}if(name==='events')loadEvents(true);if(name==='runs')loadRuns();if(name==='suites')drainSelection('report');}

function canWrite(){const d=descriptor();return writeEnabled&&d?.writable===true&&d.state==='ready'&&d.apiAvailable;}
function secretType(t){return t?.apiVersion==='v1'&&t?.kind==='Secret';}
function refreshWriteControls(){
 const t=selectedType();$('resource-create').hidden=!writeEnabled||secretType(t);$('resource-create').disabled=!canWrite()||!t;
 $('resource-edit').hidden=!writeEnabled||secretType(t);$('resource-edit').disabled=!canWrite()||!detailSnapshot||detailSnapshot.session!==selected;
 if(editor){const available=canWrite()&&editor.session===selected;$('editor-save').disabled=editor.pending||editor.uncertain||!available;$('editor-reload').disabled=editor.pending||!available;$('editor-json').disabled=editor.pending;message('editor-availability',available?'':'Session unavailable or ended. Draft retained; saving is disabled.');}
}
function dirtyEditor(){return editor&&($('editor-json').value!==editor.original||editor.pending||editor.uncertain);}
function leaveEditor(){
 if(!editor)return true;
 if(dirtyEditor()&&!window.confirm(editor.pending||editor.uncertain?'A save may have committed. Discard this draft and leave?':'Discard this unsaved JSON draft?'))return false;
 editor.controller?.abort();editor=null;editorSerial++;$('editor-json').value='';$('editor').hidden=true;refreshWriteControls();return true;
}
// Format punctuation only: numeric tokens never pass through Number/stringify.
function formatExact(raw){let out='',depth=0,quoted=false,escaped=false;const indent=()=> '  '.repeat(Math.min(depth,16));for(let i=0;i<raw.length;i++){const c=raw[i];if(quoted){out+=c;if(escaped)escaped=false;else if(c==='\\')escaped=true;else if(c==='"')quoted=false;continue;}if(c==='"'){quoted=true;out+=c;}else if(/\s/.test(c))continue;else if(c==='{'||c==='['){out+=c;depth++;if(!/^[\s]*[}\]]/.test(raw.slice(i+1,i+20)))out+='\n'+indent();}else if(c==='}'||c===']'){depth--;if(!/[{[]\s*$/.test(out))out+='\n'+indent();out+=c;}else if(c===',')out+=',\n'+indent();else if(c===':')out+=': ';else out+=c;if(out.length>2*1024*1024)return raw;}return out;}
function beginEditor(mode){
 if(!canWrite()||!selectedType()||secretType(selectedType())||!leaveEditor())return;
 const t=selectedType(),ns=t.scope==='Namespaced'?$('namespace').value:'';
 if(mode==='update'&&(!detailSnapshot||detailSnapshot.session!==selected))return;
 const source=mode==='update'?detailSnapshot.raw:JSON.stringify({apiVersion:t.apiVersion,kind:t.kind,metadata:{name:'example',...(ns?{namespace:ns}:{})}},null,2);
 editor={serial:++editorSerial,session:selected,type:t,typeIndex:Number($('types').value),namespace:ns,name:mode==='update'?detailSnapshot.name:'example',uid:mode==='update'?detailSnapshot.uid:'',rv:mode==='update'?detailSnapshot.rv:'',mode,original:formatExact(source),pending:false,uncertain:false};
 $('editor-json').value=editor.original;$('editor').hidden=false;message('editor-heading',mode==='create'?'Create resource':'Edit resource JSON');message('editor-status','Draft only. Nothing has been submitted.');editorTarget();refreshWriteControls();$('editor-json').focus();
}
function editorTarget(){if(!editor)return;message('editor-heading',editor.mode==='create'?'Create resource':'Edit resource JSON');message('editor-target','Session '+editor.session+' · '+editor.type.kind+' '+editor.type.apiVersion+' · '+(editor.namespace||'cluster scope')+' / '+editor.name+(editor.rv?' · loaded version '+editor.rv:''));$('editor-save').textContent=editor.mode==='create'?'Create':'Save';}
function draftObject(){const text=$('editor-json').value;if(new TextEncoder().encode(text).length>1024*1024)throw Error('Object exceeds the 1 MiB limit; draft retained.');let object;try{object=JSON.parse(text);}catch{throw Error('Invalid JSON; draft retained.');}if(!object||typeof object!=='object'||Array.isArray(object))throw Error('Enter one JSON object.');const m=object.metadata;if(!m||typeof m!=='object'||Array.isArray(m))throw Error('Object metadata is required.');if(object.apiVersion!==editor.type.apiVersion||object.kind!==editor.type.kind||(m.namespace||'')!==editor.namespace)throw Error('Kind, API version and namespace must match the selected target.');if(editor.mode==='update'&&(m.name!==editor.name||m.uid!==editor.uid||m.resourceVersion!==editor.rv))throw Error('Loaded name, UID and resourceVersion are fixed; load latest explicitly to change versions.');if(typeof m.name!=='string'||!m.name)throw Error('metadata.name is required.');return {text,name:m.name};}
async function saveEditor(){
 const e=editor;if(!e||e.pending||e.uncertain||!canWrite()||e.session!==selected)return;
 let draft;try{draft=draftObject();}catch(error){message('editor-status',error.message);return;}
 if(e.mode==='create')e.name=draft.name;editorTarget();
 const bytes=new Uint32Array(4);crypto.getRandomValues(bytes);const action=[...bytes].map(x=>x.toString(16).padStart(8,'0')).join('');
 const envelope={sessionId:e.session,actionId:action,operation:e.mode,apiVersion:e.type.apiVersion,kind:e.type.kind,namespace:e.namespace,name:e.name,uid:e.uid,resourceVersion:e.rv};
 const body=JSON.stringify(envelope).slice(0,-1)+',"object":'+draft.text+'}';
 e.pending=true;e.controller=new AbortController();refreshWriteControls();message('editor-status','Submitting to the selected session…');
 const timer=setTimeout(()=>e.controller.abort(),3000);
 try{
  const response=await fetch('/ui/api/v1/sessions/'+e.session+'/commands',{method:'POST',headers:{'Content-Type':'application/json','X-Dynamo-Write':'1'},body,signal:e.controller.signal,cache:'no-store'});
  const data=await response.json();if(editor!==e||selected!==e.session)return;
  const r=data.result;
  if(!r){if(response.status>=500)throw Error('unknown');message('editor-status','Rejected: '+(data.error?.code||'request unavailable')+'. Draft retained.');return;}
  if(r.sessionId!==e.session||r.actionId!==action||!['committed','no-op','rejected','committed-but-session-failed'].includes(r.disposition))throw Error('unknown');
  if(r.disposition==='rejected'){message('editor-status','Rejected ('+r.status+'): '+r.reason+'. Draft retained.'+(r.status===409?' Load latest explicitly before editing the new version.':''));return;}
  if(!r.requestId)throw Error('unknown');
  if(r.sessionFailed||r.disposition==='committed-but-session-failed'){e.uncertain=true;message('editor-status',(r.disposition==='no-op'?'Accepted no-op; resource unchanged; session failed / recording incomplete.':'Committed; session failed / recording incomplete.')+' Do not replay. Inspect the outcome. Request '+r.requestId);return;}
  if(typeof r.objectJson!=='string'||!r.uid||!r.resourceVersion)throw Error('unknown');
  e.mode='update';e.uid=r.uid;e.rv=r.resourceVersion;e.original=formatExact(r.objectJson);$('editor-json').value=e.original;e.uncertain=false;editorTarget();
  message('editor-status',(r.disposition==='no-op'?'Accepted no-op; resource unchanged.':'API commit accepted.')+' Version '+r.resourceVersion+' · request '+r.requestId+'. Operator reconciliation has not been inferred.');
  loadObjects();
 }catch(error){if(editor===e&&selected===e.session){e.uncertain=true;message('editor-status','Outcome unknown. The API may have committed. Draft retained; inspect the outcome before another explicit action.');}}
 finally{clearTimeout(timer);if(editor===e){e.pending=false;refreshWriteControls();}}
}
async function reloadEditor(){
 const e=editor;if(!e||e.pending||!canWrite()||e.session!==selected)return;
 if(!window.confirm('Load the current API object and discard this draft if it exists? No write will be retried.'))return;
 const q=new URLSearchParams({apiVersion:e.type.apiVersion,kind:e.type.kind,name:e.name});if(e.namespace)q.set('namespace',e.namespace);
 e.pending=true;e.controller=new AbortController();refreshWriteControls();const timer=setTimeout(()=>e.controller.abort(),3000);
 try{const response=await fetch('/ui/api/v1/sessions/'+e.session+'/objects?'+q,{signal:e.controller.signal,cache:'no-store'});const data=await response.json();if(editor!==e||selected!==e.session)return;if(response.status===404){e.uncertain=false;message('editor-status','No current object found. This does not prove an earlier save never committed. Draft retained; another action must be explicit.');return;}if(!response.ok||data.sessionId!==e.session||typeof data.objectJson!=='string')throw Error('unavailable');e.mode='update';e.uid=data.uid;e.rv=data.resourceVersion;e.original=formatExact(data.objectJson);$('editor-json').value=e.original;e.uncertain=false;editorTarget();message('editor-status','Latest API object loaded. Previous draft discarded; nothing submitted.');}catch{if(editor===e)message('editor-status','Inspection unavailable. Draft retained; no write was retried.');}finally{clearTimeout(timer);if(editor===e){e.pending=false;refreshWriteControls();}}
}
$('resource-create').onclick=()=>beginEditor('create');$('resource-edit').onclick=()=>beginEditor('update');$('editor-save').onclick=saveEditor;$('editor-reload').onclick=reloadEditor;$('editor-close').onclick=leaveEditor;
$('editor-json').oninput=()=>{if(editor?.mode==='create'){try{const d=JSON.parse($('editor-json').value);if(typeof d?.metadata?.name==='string'){editor.name=d.metadata.name;editorTarget();}}catch{}}};
window.addEventListener('beforeunload',event=>{if(dirtyEditor()){event.preventDefault();event.returnValue='';}});
for(const b of document.querySelectorAll('[data-view]'))b.onclick=()=>view(b.dataset.view);
$('catalog-refresh').onclick=loadCatalog;$('resource-refresh').onclick=()=>{if(types.length)loadObjects();else loadTypes();};$('types').onchange=typeChanged;$('namespace').onchange=typeChanged;$('detail-refresh').onclick=()=>loadDetail(objectName);$('events-more').onclick=()=>loadEvents(true);$('runs-more').onclick=loadRuns;$('current-run').onclick=()=>{if(currentRun)loadReport(currentRun);};
$('session-select').onchange=()=>selectSession($('session-select').value);
$('browse-runs').onclick=()=>view('runs');$('browse-resources').onclick=()=>view('resources');
$('report-refresh').onclick=()=>{if(chosenRun)loadReport(chosenRun);};
$('scenario-search').oninput=renderLedger;$('scenario-outcome').onchange=renderLedger;
$('jump-details').onclick=()=>{$('scenario-inspector').focus();$('scenario-inspector').scrollIntoView({block:'start',behavior:'instant'});};
document.addEventListener('visibilitychange',()=>{if(document.hidden){invalidate(true);for(const job of jobs.values())job.controller.abort();}else{loadCatalog();for(const view of pendingSelections.keys())drainSelection(view);}});
let tick=0;setInterval(()=>{if(document.hidden)return;tick+=500;if(tick<state.retry)return;tick=0;loadCatalog();if(activeView==='resources'&&live()){if(types.length)loadObjects();else loadTypes();}if(activeView==='events'&&live())loadEvents();},500);
loadCatalog();
