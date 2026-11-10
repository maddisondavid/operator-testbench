import importlib.util
from pathlib import Path
import tempfile
import unittest
import signal
import subprocess
import sys
import json
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('smoke',Path(__file__).resolve().parents[2]/'scripts/agent-smoke.py')
smoke=importlib.util.module_from_spec(spec);spec.loader.exec_module(smoke)
class SnapshotTest(unittest.TestCase):
    def test_inputs_only_permissions_and_unchanged_source(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);source=root/'source';target=root/'target';source.mkdir();target.mkdir()
            for name in ['runs','sessions','.git']:
                (source/name).mkdir();(source/name/'stale').write_text('old')
            (source/'input').write_text('fresh');(source/'input').chmod(0o755)
            manifest=smoke.snapshot(source,target)
            self.assertEqual(list(manifest),['input']);self.assertEqual((target/'input').stat().st_mode&0o777,0o755)
            self.assertTrue((source/'runs/stale').exists())
    def test_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);source=root/'source';target=root/'target';source.mkdir();target.mkdir()
            (source/'escape').symlink_to('/etc/passwd')
            with self.assertRaises(ValueError):smoke.snapshot(source,target)
class FinalCancellationTest(unittest.TestCase):
    def test_signal_during_cleanup_cannot_pass_or_replace_failure(self):
        class Done:
            returncode=0
            def poll(self):return 0
            def wait(self,timeout=None):return 0
        for actual in (0,1,2,3):
            with self.subTest(actual=actual),tempfile.TemporaryDirectory() as d:
                root=Path(d);source=root/'inputs';source.mkdir();(source/'a').write_text('a');(source/'config').mkdir();(source/'config/operator.yaml').write_text('api: {}')
                def docker(*args,**kwargs):
                    if args[0]=='inspect':return subprocess.CompletedProcess(args,0,stdout=json.dumps({'Running':False,'ExitCode':actual}))
                    if args[0]=='image':return subprocess.CompletedProcess(args,0,stdout='sha256:'+'a'*64)
                    if args[0]=='stop':signal.raise_signal(signal.SIGINT)
                    return subprocess.CompletedProcess(args,0,stdout='')
                with patch.object(smoke.socket,'socket'),patch.object(smoke,'validate_profile',lambda:None),patch.object(sys,'argv',['smoke','--host-address','198.51.100.10','--workspace',str(source),'--evidence',str(root/'evidence')]),patch.object(smoke,'docker',docker),patch.object(smoke.subprocess,'Popen',lambda *a,**k:Done()),patch.object(smoke.subprocess,'run',lambda *a,**k:subprocess.CompletedProcess(a,1)):
                    self.assertEqual(smoke.main(),actual or 1)
if __name__=='__main__':unittest.main()
