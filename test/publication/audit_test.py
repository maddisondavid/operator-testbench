import importlib.util,json,os,subprocess,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
ROOT=Path(__file__).resolve().parents[2]
s=importlib.util.spec_from_file_location('audit',ROOT/'scripts/audit-git.py');m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
class AuditTests(unittest.TestCase):
 def setup_repo(self,p):
  subprocess.run(['git','init','-q',str(p)],check=True)
  (p/'test/publication').mkdir(parents=True);(p/'test/publication/scan-exceptions.json').write_text('[]')
  (p/'README.md').write_text('safe text\n');(p/'.gitignore').write_text('private/\n')
 def test_actual_git_set_checkout_and_index_untouched(self):
  with tempfile.TemporaryDirectory() as d:
   p=Path(d)/'repo';self.setup_repo(p);(p/'private').mkdir();(p/'private/old').write_text('local')
   result=m.audit(p,Path(d)/'audit');self.assertEqual(result['indexEntriesBefore'],0);self.assertEqual(result['reachableCommits'],0)
   self.assertFalse((p/'.git/index').exists());self.assertFalse((Path(d)/'audit/checkout/private').exists())
   self.assertEqual((Path(d)/'audit/checkout/README.md').read_text(),'safe text\n')
 def test_new_eligible_file_not_in_old_allowlist_rejects(self):
  for value in ('password="'+'DO-NOT-DISCLOSE-CANARY'+'"','http://'+'10.33.44.55'):
   with tempfile.TemporaryDirectory() as d:
    p=Path(d)/'repo';self.setup_repo(p);(p/'new-input.txt').write_text(value)
    with self.assertRaises(m.Rejected) as error:m.audit(p,Path(d)/'audit')
    self.assertNotIn(value,str(error.exception));self.assertFalse((Path(d)/'audit').exists())
 def test_tracked_ignored_and_staged_bytes_cannot_hide(self):
  with tempfile.TemporaryDirectory() as d:
   p=Path(d)/'repo';self.setup_repo(p);(p/'private').mkdir();f=p/'private/value';f.write_text('password="'+'STAGED-SENSITIVE-CANARY'+'"')
   subprocess.run(['git','-C',str(p),'add','-f','private/value'],check=True);before=(p/'.git/index').read_bytes();f.write_text('safe')
   with self.assertRaises(m.Rejected):m.audit(p,Path(d)/'audit')
   self.assertEqual((p/'.git/index').read_bytes(),before)
 def test_history_checked_even_after_worktree_deletion(self):
  with tempfile.TemporaryDirectory() as d:
   p=Path(d)/'repo';self.setup_repo(p);f=p/'old';f.write_text('password="'+'HISTORICAL-SENSITIVE-CANARY'+'"')
   subprocess.run(['git','-C',str(p),'add','.'],check=True)
   subprocess.run(['git','-C',str(p),'-c','user.name=Synthetic','-c','user.email=synthetic@example.invalid','commit','-qm','synthetic fixture'],check=True)
   f.unlink()
   with self.assertRaises(m.Rejected):m.audit(p,Path(d)/'audit')
 def test_fifo_and_symlink_reject(self):
  for mode in ('fifo','symlink'):
   with tempfile.TemporaryDirectory() as d:
    p=Path(d)/'repo';self.setup_repo(p)
    if mode=='fifo':
     (p/'new').write_text('safe');subprocess.run(['git','-C',str(p),'add','new'],check=True);(p/'new').unlink();os.mkfifo(p/'new')
    else:(p/'new').symlink_to(p/'README.md')
    with self.assertRaises((m.Rejected,m.safety.Rejected,OSError)):m.audit(p,Path(d)/'audit')
if __name__=='__main__':unittest.main()
