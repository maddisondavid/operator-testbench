#!/usr/bin/env python3
"""Deploy a copied workspace in local Docker with explicit reachable endpoints."""
import argparse, importlib.util, json, os, re, signal, subprocess, tempfile, uuid
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
s=importlib.util.spec_from_file_location('smoke',ROOT/'scripts/agent-smoke.py');m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
def start_owned(args):
 cid=m.docker(*args,capture_output=True,text=True).stdout.strip()
 try:m.docker('start',cid,capture_output=True,text=True)
 except subprocess.SubprocessError:
  m.docker('rm','-f',cid,capture_output=True,text=True)
  raise
 return cid

def run_helper(args):
 token=uuid.uuid4().hex;name='dynamo-configure-'+token;label='dynamo.configure-owner'
 def cancel(signum,frame):raise InterruptedError('configuration cancelled')
 handlers={sig:signal.signal(sig,cancel) for sig in (signal.SIGINT,signal.SIGTERM)}
 try:
  cid=m.docker('create','--name',name,'--label',label+'='+token,'--log-driver','none',*args,capture_output=True,text=True).stdout.strip()
  m.docker('start','-a',cid,timeout=90)
  code=m.docker('inspect',cid,'--format','{{.State.ExitCode}}',capture_output=True,text=True).stdout.strip()
  if code!='0':raise subprocess.CalledProcessError(1,['configuration helper'])
 finally:
  # Ignore repeated cancellation during the single bounded cleanup attempt.
  for sig in handlers:signal.signal(sig,signal.SIG_IGN)
  try:
   found=m.docker('inspect',name,timeout=5,capture_output=True,text=True)
   rows=json.loads(found.stdout)
   if len(rows)==1 and rows[0].get('Config',{}).get('Labels',{}).get(label)==token:
    m.docker('rm','-f',rows[0]['Id'],timeout=5,capture_output=True,text=True)
  except subprocess.CalledProcessError:
   # A failed create may have left no object. Other cleanup failures propagate.
   if 'cid' in locals():raise
  finally:
   for sig,handler in handlers.items():signal.signal(sig,handler)

def main():
 p=argparse.ArgumentParser(description=__doc__)
 p.add_argument('--workspace',required=True);p.add_argument('--host-address',default=os.environ.get('DYNAMO_HOST_ADDRESS'))
 p.add_argument('--http-port',type=int,default=18080);p.add_argument('--api-port',type=int,default=18081)
 p.add_argument('--image',default='operator-dynamo:agent-smoke');p.add_argument('--name',default='dynamo-shared-editor')
 p.add_argument('--evidence',default='evidence/deployments');p.add_argument('--ui-write',action='store_true')
 a=p.parse_args()
 try:m.validate_endpoints(a.host_address,a.http_port,a.api_port);m.validate_profile()
 except ValueError as e:p.error(str(e))
 if not re.fullmatch(r'dynamo-[a-z0-9][a-z0-9-]{0,50}',a.name):p.error('name must be a bounded dynamo- name')
 source=Path(a.workspace).resolve(strict=True);base=Path(a.evidence).resolve()
 if base==source or source in base.parents:p.error('evidence must be outside saved inputs')
 base.mkdir(parents=True,exist_ok=True);run=Path(tempfile.mkdtemp(prefix='deployment-',dir=base));work=run/'workspace';work.mkdir()
 original=m.snapshot(source,work);(run/'original-inputs.json').write_text(json.dumps(original,sort_keys=True))
 prefix=os.environ.get('DYNAMO_SMOKE_CACHE_PREFIX','dynamo-go')
 if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}',prefix):p.error('invalid cache prefix')
 run_helper(['--network','none','--user',f'{os.getuid()}:{os.getgid()}','-v',f'{ROOT}:/src:ro','-v',f'{run}:{run}','-w','/src','-v',prefix+'-modules:/go/pkg/mod','-v',prefix+'-cache:/tmp/gocache','-e','GOCACHE=/tmp/gocache/agent-smoke','golang:1.26.5-bookworm','go','run','./test/agent-smoke','--configure',str(work),a.host_address,str(a.api_port)])
 effective=dict(original);effective['config/operator.yaml']=m.hashlib.sha256((work/'config/operator.yaml').read_bytes()).hexdigest();(run/'inputs.json').write_text(json.dumps(effective,sort_keys=True))
 image=m.docker('image','inspect',a.image,'--format','{{.Id}}',capture_output=True,text=True).stdout.strip()
 args=['create','--log-driver','local','--log-opt','max-size=1m','--log-opt','max-file=1','--log-opt','compress=false','--name',a.name,'--pull','never','--user',f'{os.getuid()}:{os.getgid()}','--group-add',str(os.stat('/var/run/docker.sock').st_gid),'-v','/var/run/docker.sock:/var/run/docker.sock','-v',f'{work}:/state','-p',f'{a.host_address}:{a.http_port}:8080','-p',f'{a.host_address}:{a.api_port}:{a.api_port}',image,'--workdir','/state','--listen','0.0.0.0:8080','--ui','--ui-origin',f'http://{a.host_address}:{a.http_port}']
 if a.ui_write:args.append('--ui-write')
 # Docker rejects duplicate names/bindings; never remove an existing deployment.
 cid=start_owned(args)
 result={'container':cid,'name':a.name,'image':image,'workspace':str(work),'url':f'http://{a.host_address}:{a.http_port}/ui/'}
 (run/'deployment.json').write_text(json.dumps(result,indent=2));print(json.dumps(result))
if __name__=='__main__':
 try:main()
 except (ValueError,OSError,subprocess.SubprocessError):raise SystemExit('Deployment failed; inspect owned Docker state (no automatic replacement).')
