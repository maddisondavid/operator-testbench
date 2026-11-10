import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch
import os
import subprocess
import json
s=importlib.util.spec_from_file_location('runner',Path(__file__).resolve().parents[2]/'scripts/agent-smoke.py');m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
class EndpointTests(unittest.TestCase):
 def test_validation(self):
  for host,a,b in [('localhost',18088,18089),('::1',18088,18089),('0.0.0.0',18088,18089),('127.0.0.1',18088,18089),('224.0.0.1',18088,18089),('255.255.255.255',18088,18089),('198.51.100.10',18088,8080),('198.51.100.10',18088,18088),('198.51.100.10',1,18089),('198.51.100.10',18088,65536)]:
   with self.subTest(host=host,a=a,b=b),self.assertRaises(ValueError):m.validate_endpoints(host,a,b)
  m.validate_endpoints('198.51.100.20',8080,18093)
 def test_remote_profile_rejected(self):
  with patch.dict(os.environ,{'DOCKER_HOST':'tcp://example.invalid:2375'}),self.assertRaises(ValueError):m.validate_profile()
  with patch.dict(os.environ,{},clear=True),patch.object(m,'docker',lambda *a,**k:subprocess.CompletedProcess(a,0,stdout='tcp://example.invalid:2375')),self.assertRaises(ValueError):m.validate_profile()
if __name__=='__main__':unittest.main()
