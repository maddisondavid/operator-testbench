import os, subprocess, unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
class EndpointAdmission(unittest.TestCase):
 def test_missing_and_injection_before_docker(self):
  env={**os.environ};env.pop('DYNAMO_HOST_ADDRESS',None)
  for script,args in [('agent-smoke.py',[]),('deploy-source.py',['--workspace','examples/agent-smoke'])]:
   for extra in [[],['--host-address','$(touch SHOULD_NOT_EXIST)'],['--host-address','0.0.0.0']]:
    p=subprocess.run(['python3',str(ROOT/'scripts'/script),*args,*extra],cwd=ROOT,env=env,capture_output=True,text=True,timeout=3)
    self.assertNotEqual(p.returncode,0);self.assertFalse((ROOT/'SHOULD_NOT_EXIST').exists());self.assertNotIn('Traceback',p.stderr)
if __name__=='__main__':unittest.main()
