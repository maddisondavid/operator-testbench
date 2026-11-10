#!/usr/bin/env python3
"""Audit every normal-staging file and Git history without changing the real index."""
import argparse, hashlib, importlib.util, json, os, shutil, subprocess, tempfile
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location('safety',ROOT/'scripts/source-safety.py');safety=importlib.util.module_from_spec(spec);spec.loader.exec_module(safety)
MAX_FILES=10000;MAX_TOTAL=64*1024*1024
class Rejected(Exception):pass

def git(root,*args,env=None):
 p=subprocess.run(['git','-C',str(root),*args],env=env,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=30)
 if p.returncode:raise Rejected('Git operation failed')
 if len(p.stdout)>MAX_TOTAL:raise Rejected('Git output bound')
 return p.stdout

def scan(data,path,exceptions,used):
 for finding in safety.findings(data):
  matched=next((i for i,e in enumerate(exceptions) if e.get('source')==path and all(e.get(k)==v for k,v in finding.items()) and e.get('reason')),None)
  if matched is None:raise Rejected('unclassified content finding: '+finding['class'])
  used.add(matched)

def objects(root,ids,exceptions):
 total=0;count=0
 for oid,path in ids:
  count+=1
  if count>MAX_FILES:raise Rejected('Git object count bound')
  size=int(git(root,'cat-file','-s',oid))
  total+=size
  if size>safety.MAX_FILE or total>MAX_TOTAL:raise Rejected('Git object size bound')
  data=git(root,'cat-file','-p',oid)
  if b'\0' in data:raise Rejected('binary Git object')
  try:data.decode('utf-8')
  except UnicodeError:raise Rejected('non-text Git object')
  scan(data,path,exceptions,set())

def staged(root,env):
 entries=[]
 for row in git(root,'ls-files','--stage','-z',env=env).split(b'\0'):
  if not row:continue
  meta,raw=row.split(b'\t',1);mode,oid,stage=meta.decode().split();path=raw.decode('utf-8')
  if stage!='0' or mode not in ('100644','100755') or not safety.safe_path(path):raise Rejected('unsupported Git entry')
  entries.append((mode,oid,path))
 if len(entries)>MAX_FILES:raise Rejected('Git entry bound')
 return entries

def audit(root,out):
 if os.path.lexists(out):raise Rejected('output already exists')
 exceptions=json.loads(safety.read(root,'test/publication/scan-exceptions.json')[0])
 if not isinstance(exceptions,list) or len(exceptions)>200:raise Rejected('exception bound')
 paths=set(p.decode('utf-8') for p in git(root,'ls-files','--cached','--others','--exclude-standard','-z').split(b'\0') if p)
 if len(paths)>MAX_FILES:raise Rejected('eligible file bound')
 total=0;used=set()
 for path in sorted(paths):
  if not os.path.lexists(root/path):continue # Normal staging records tracked deletions.
  data,_=safety.read(root,path);total+=len(data)
  if total>MAX_TOTAL:raise Rejected('eligible byte bound')
  scan(data,path,exceptions,used)
 # Audit staged bytes too: a clean worktree must not conceal a sensitive index.
 current=staged(root,None);objects(root,[(oid,path) for mode,oid,path in current],exceptions)
 refs=git(root,'rev-list','--all').splitlines()
 history=[]
 for row in git(root,'rev-list','--objects','--all').splitlines():
  oid,_,path=row.partition(b' ')
  kind=git(root,'cat-file','-t',oid.decode()).strip()
  if kind in (b'blob',b'commit',b'tag'):history.append((oid.decode(),path.decode('utf-8') if path else ''))
 objects(root,history,exceptions)
 with tempfile.TemporaryDirectory(prefix='dynamo-git-index-') as tmp:
  env=dict(os.environ);env['GIT_INDEX_FILE']=str(Path(tmp)/'index')
  # HEAD is intentionally unnecessary for a new repository. git add includes
  # ignored tracked entries only when seeded from the actual current index.
  index_path=Path(os.fsdecode(git(root,'rev-parse','--git-path','index')).strip())
  if not index_path.is_absolute():index_path=root/index_path
  if index_path.exists():shutil.copyfile(index_path,env['GIT_INDEX_FILE'])
  else:git(root,'read-tree','--empty',env=env)
  git(root,'add','-A','--','.',env=env)
  entries=staged(root,env);items=[];manifest=[];total=0;used=set()
  for mode,oid,path in entries:
   size=int(git(root,'cat-file','-s',oid));total+=size
   if size>safety.MAX_FILE or total>MAX_TOTAL:raise Rejected('candidate byte bound')
   data=git(root,'cat-file','blob',oid)
   if b'\0' in data:raise Rejected('binary candidate')
   data.decode('utf-8');scan(data,path,exceptions,used)
   items.append((path,data,mode));manifest.append({'path':path,'mode':mode,'gitBlob':oid,'sha256':hashlib.sha256(data).hexdigest(),'bytes':len(data)})
  if len(used)!=len(exceptions):raise Rejected('stale scan exception')
  tree=git(root,'write-tree',env=env).decode().strip()
 # Materialize only already checked regular Git blobs into a new private output.
 out.mkdir(parents=True,exist_ok=False);checkout=out/'checkout';checkout.mkdir()
 try:
  for path,data,mode in items:
   dest=checkout/path;dest.parent.mkdir(parents=True,exist_ok=True);dest.write_bytes(data);dest.chmod(0o755 if mode=='100755' else 0o644)
  encoded=(json.dumps(manifest,sort_keys=True,indent=2)+'\n').encode();(out/'manifest.json').write_bytes(encoded)
  result={'tree':tree,'files':len(items),'manifestSHA256':hashlib.sha256(encoded).hexdigest(),'indexEntriesBefore':len(current),'reachableCommits':len(refs),'historyObjectsScanned':len(history),'boundary':'normal git add -A candidate; no real index, commit, ref or remote changed'}
  (out/'result.json').write_text(json.dumps(result,indent=2)+'\n')
 except BaseException:
  shutil.rmtree(out);raise
 return result

def main():
 p=argparse.ArgumentParser(description=__doc__);p.add_argument('--output',required=True);a=p.parse_args()
 try:
  result=audit(ROOT,Path(os.path.abspath(a.output)));print(json.dumps(result))
 except (Rejected,safety.Rejected,OSError,ValueError,KeyError,subprocess.SubprocessError) as e:
  print('Git audit rejected: '+(str(e) if isinstance(e,(Rejected,safety.Rejected)) else 'invalid/unreadable input'));raise SystemExit(2)
if __name__=='__main__':main()
