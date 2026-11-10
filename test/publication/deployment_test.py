import importlib.util, json, subprocess, unittest
from pathlib import Path
from unittest.mock import patch
ROOT=Path(__file__).resolve().parents[2]
s=importlib.util.spec_from_file_location('deployment',ROOT/'scripts/deploy-source.py');m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
class OwnedCleanup(unittest.TestCase):
 def test_failed_start_removes_only_new_id_and_collision_removes_nothing(self):
  for failure in ('create','start',None):
   calls=[]
   def docker(*args,**kwargs):
    calls.append(args)
    if args[0]==failure:raise subprocess.CalledProcessError(1,args)
    return subprocess.CompletedProcess(args,0,stdout='new-owned-id\n')
   with patch.object(m.m,'docker',docker):
    if failure:
     with self.assertRaises(subprocess.CalledProcessError):m.start_owned(['create','--name','dynamo-existing'])
    else:self.assertEqual(m.start_owned(['create']),'new-owned-id')
   removals=[x for x in calls if x[0]=='rm']
   self.assertEqual(removals,[('rm','-f','new-owned-id')] if failure=='start' else [])
 def test_configuration_helper_cleanup_on_timeout_cancel_and_success(self):
  for failure in ('create','start','cancel',None):
   calls=[];owner=None
   def docker(*args,**kwargs):
    nonlocal owner
    calls.append(args)
    if args[0]=='create':
     owner=args[args.index('--label')+1].split('=',1)[1]
     if failure=='create':raise subprocess.TimeoutExpired('create',30)
    if args[0]=='start':
     if failure=='start':raise subprocess.TimeoutExpired('start',90)
     if failure=='cancel':raise InterruptedError('cancel')
    if args[0]=='inspect':
     value='0' if '--format' in args else json.dumps([{'Id':'owned-helper','Config':{'Labels':{'dynamo.configure-owner':owner}}}])
    else:value='owned-helper'
    return subprocess.CompletedProcess(args,0,stdout=value)
   with patch.object(m.m,'docker',docker):
    if failure:
     with self.assertRaises((subprocess.TimeoutExpired,InterruptedError)):m.run_helper(['image'])
    else:m.run_helper(['image'])
   self.assertEqual([x for x in calls if x[0]=='rm'],[('rm','-f','owned-helper')])
if __name__=='__main__':unittest.main()
