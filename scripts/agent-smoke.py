#!/usr/bin/env python3
"""Run saved Dynamo inputs once in Docker; never reuse a previous report."""
import argparse
import ipaddress
import platform
import re
import socket
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]

def docker(*args, timeout=30, **kwargs):
    return subprocess.run(['docker', *args], timeout=timeout, check=True, **kwargs)

def validate_endpoints(host,http_port,api_port):
    if not host:
        raise ValueError("explicit --host-address or DYNAMO_HOST_ADDRESS is required")
    try:
        address=ipaddress.IPv4Address(host)
    except ValueError:
        raise ValueError('host-address must be an explicit IPv4 address')
    if address.is_unspecified or address.is_loopback or address.is_multicast or address.is_reserved or address.is_link_local:
        raise ValueError('host-address must be a non-loopback unicast host address')
    if any(p<1024 or p>65535 for p in (http_port,api_port)) or http_port==api_port or api_port==8080:
        raise ValueError('ports must be distinct nonprivileged ports; API must not collide with internal HTTP listener')

def validate_profile():
    if platform.system()!='Linux' or platform.machine() not in ('x86_64','amd64'):
        raise ValueError('supported profile is Linux/amd64 local Docker')
    if os.environ.get('DOCKER_HOST','unix:///var/run/docker.sock')!='unix:///var/run/docker.sock' or os.environ.get('DOCKER_CONTEXT','default')!='default' or os.environ.get('DOCKER_TLS_VERIFY') or os.environ.get('DOCKER_TLS'):
        raise ValueError('only the local Unix-socket Docker profile is supported')
    context=docker('context','inspect','--format','{{.Endpoints.docker.Host}}',capture_output=True,text=True).stdout.strip()
    if context!='unix:///var/run/docker.sock':
        raise ValueError('active Docker context must use the local Unix socket')
    info=json.loads(docker('info','--format','{{json .}}',capture_output=True,text=True).stdout)
    if info.get('OSType')!='linux' or info.get('Architecture') not in ('x86_64','amd64') or 'docker desktop' in info.get('OperatingSystem','').lower():
        raise ValueError('supported daemon is Linux/amd64 Engine, not Docker Desktop')

def snapshot(source, target):
    manifest = {}
    total = 0
    visited = 0
    path_bytes = 0
    for parent, dirs, files in os.walk(source, followlinks=False):
        rel = Path(parent).relative_to(source)
        if len(rel.parts) > 64:
            raise ValueError('input depth limit')
        if rel == Path('.'):
            dirs[:] = [d for d in dirs if d not in ('runs', 'sessions', '.git')]
            files = [f for f in files if f != '.dynamo.lock']
        for name in dirs + files:
            visited += 1
            path_bytes += len(os.fsencode(rel / name))
            if path_bytes > 8 * 1024 * 1024:
                raise ValueError('input path limit')
            path = Path(parent) / name
            info = path.lstat()
            if visited > 100000 or stat.S_ISLNK(info.st_mode):
                raise ValueError('unsafe or excessive inputs')
            dest = target / rel / name
            if stat.S_ISDIR(info.st_mode):
                dest.mkdir(parents=True, exist_ok=True)
                continue
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise ValueError('inputs must be regular, singly linked files')
            total += info.st_size
            if total > 256 * 1024 * 1024:
                raise ValueError('input byte limit')
            dest.parent.mkdir(parents=True, exist_ok=True)
            with path.open('rb') as stream:
                data = stream.read(info.st_size + 1)
            if len(data) != info.st_size:
                raise ValueError('input changed while copying')
            dest.write_bytes(data)
            os.chmod(dest, stat.S_IMODE(info.st_mode) & 0o777)
            after = path.stat()
            if (info.st_ino, info.st_size, info.st_mtime_ns) != (after.st_ino, after.st_size, after.st_mtime_ns):
                raise ValueError('input changed while copying')
            manifest[(rel / name).as_posix()] = hashlib.sha256(dest.read_bytes()).hexdigest()
    return manifest

def execute(completion):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--workspace', default='examples/agent-smoke')
    parser.add_argument('--tests', default='tests/smoke')
    parser.add_argument('--image', default='operator-dynamo:agent-smoke')
    parser.add_argument('--evidence', default='evidence/agent-smoke-runs')
    parser.add_argument('--timeout', type=int, default=300)
    parser.add_argument('--host-address', default=os.environ.get('DYNAMO_HOST_ADDRESS'))
    parser.add_argument('--http-port', type=int, default=18088)
    parser.add_argument('--api-port', type=int, default=18089)
    args = parser.parse_args()
    try:
        validate_endpoints(args.host_address,args.http_port,args.api_port)
        validate_profile()
    except ValueError as error:
        parser.error(str(error))
    cache_prefix=os.environ.get('DYNAMO_SMOKE_CACHE_PREFIX','dynamo-go')
    if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}',cache_prefix):
        parser.error('invalid DYNAMO_SMOKE_CACHE_PREFIX')
    source = Path(args.workspace).resolve(strict=True)
    selection = Path(args.tests)
    if selection.is_absolute() or '..' in selection.parts or not selection.parts:
        parser.error('tests must be a contained relative selection')
    if not 1 <= args.timeout <= 3600:
        parser.error('timeout must be 1..3600 seconds')
    evidence = Path(args.evidence).resolve()
    if evidence == source or source in evidence.parents:
        parser.error('evidence must be outside saved inputs')
    evidence.mkdir(parents=True, exist_ok=True)
    run = Path(tempfile.mkdtemp(prefix='invocation-', dir=evidence))
    work = run / 'workspace'
    work.mkdir()
    name = 'dynamo-smoke-' + uuid.uuid4().hex
    started = time.monotonic()
    (run / 'started.txt').write_text(datetime.now(timezone.utc).isoformat().replace('+00:00', 'Z'))
    cancelled = False
    launched = False
    validator_launched = False
    code = None
    def cancel(signum, frame):
        nonlocal cancelled
        cancelled = True
        completion["cancelled"] = True
    old = {sig: signal.signal(sig, cancel) for sig in (signal.SIGINT, signal.SIGTERM)}
    def helper(arguments):
        nonlocal validator_launched
        validator_launched = True
        docker('create', '--rm', '--name', name+'-validate', '--pull', 'never', '--network', 'none',
            '--user', f'{os.getuid()}:{os.getgid()}', '-v', f'{ROOT}:/src:ro', '-v', f'{run}:{run}', '-w', '/src',
            '-v', cache_prefix+'-modules:/go/pkg/mod', '-v', cache_prefix+'-cache:/tmp/gocache', '-e', 'GOCACHE=/tmp/gocache/agent-smoke',
            'golang:1.26.5-bookworm', *arguments, stdout=subprocess.DEVNULL)
        proc = subprocess.Popen(['docker','start','-a',name+'-validate'])
        deadline=time.monotonic()+60
        while proc.poll() is None:
            if cancelled or time.monotonic()>=deadline:
                docker('rm','-f',name+'-validate',timeout=10,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
                proc.wait(timeout=10)
                raise ValueError('helper cancelled or deadline expired')
            time.sleep(.1)
        return proc.returncode
    try:
        docker('image','inspect','golang:1.26.5-bookworm',stdout=subprocess.DEVNULL)
        if source == ROOT/'examples/agent-smoke':
            binary=source/'operator-src/operator'
            if not binary.is_file() or hashlib.sha256(binary.read_bytes()).hexdigest()!='294516d657d3c00d9be8f83561cc4c06142af3b4de1badd37da21b1fed56d2ae':
                raise ValueError('run prepare-agent-smoke.sh once first')
        if helper(['go','build','-buildvcs=false','-o',str(run/'validate'),'./test/agent-smoke'])!=0:
            raise ValueError('validator prerequisites unavailable')
        manifest = snapshot(source, work)
        (run / 'original-inputs.json').write_text(json.dumps(manifest, sort_keys=True))
        if helper([str(run/'validate'),'--configure',str(work),args.host_address,str(args.api_port)])!=0:
            raise ValueError('configuration override failed')
        manifest['config/operator.yaml']=hashlib.sha256((work/'config/operator.yaml').read_bytes()).hexdigest()
        (run / 'inputs.json').write_text(json.dumps(manifest, sort_keys=True))
        (run / 'endpoints.txt').write_text(f'host={args.host_address}\nhttp={args.http_port}\napi={args.api_port}\n')
        # This is only an early diagnostic; Docker still owns actual binding.
        for port in (args.http_port,args.api_port):
            with socket.socket() as probe:
                probe.bind((args.host_address,port))
        (run / 'selection.txt').write_text(selection.as_posix() + '\n')
        image = docker('image', 'inspect', '--format', '{{.Id}}', args.image, capture_output=True, text=True).stdout.strip()
        (run / 'dynamo-image.txt').write_text(image + '\n')
        print(f'Evidence: {run}', flush=True)
        print(f'Preparation: {time.monotonic()-started:.3f}s', flush=True)
        if cancelled:
            return 1
        launched = True
        docker('run', '-d', '--name', name, '--pull', 'never', '--log-driver', 'local', '--log-opt', 'max-size=1m', '--log-opt', 'max-file=1', '--log-opt', 'compress=false',
               '--user', f'{os.getuid()}:{os.getgid()}', '--group-add', str(os.stat('/var/run/docker.sock').st_gid),
               '-v', '/var/run/docker.sock:/var/run/docker.sock', '-v', f'{work}:/state',
               '-p', f'{args.host_address}:{args.http_port}:8080', '-p', f'{args.host_address}:{args.api_port}:{args.api_port}', image,
               '--workdir', '/state', '--listen', '0.0.0.0:8080', '--tests', '/state/'+selection.as_posix(), '--verbose',
               stdout=subprocess.DEVNULL)
        launched = True
        execution = time.monotonic()
        while True:
            state = json.loads(docker('inspect', '--format', '{{json .State}}', name, capture_output=True, text=True).stdout)
            if not state['Running']:
                code = state['ExitCode']
                break
            if cancelled or time.monotonic() - execution >= args.timeout:
                cancelled = True
                docker('stop', '--signal', 'SIGINT', '-t', '6', name, timeout=10, stdout=subprocess.DEVNULL)
            time.sleep(.1)
        print(f'Execution including Runtime build/cleanup: {time.monotonic()-execution:.3f}s', flush=True)
        (run / 'process-exit.txt').write_text(str(code)+'\n')
        with (run / 'console.log').open('wb') as log:
            docker('logs', name, stdout=log, stderr=subprocess.STDOUT)
        if cancelled:
            print(f'Cancelled; Dynamo exit: {code}; partial evidence: {run}', flush=True)
            return code if code in (1,2,3) else 1
        validation_started=time.monotonic()
        helper_result = helper([str(run / 'validate'), str(work), selection.as_posix(), str(code)])
        print(f'Evidence validation: {time.monotonic()-validation_started:.3f}s',flush=True)
        if code != 0:
            print(f'Dynamo exit: {code}; partial evidence: {run}', flush=True)
            return code if code in (1,2,3) else 3
        return 1 if cancelled else (0 if helper_result == 0 else 3)
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        category='endpoint binding or filesystem unavailable' if isinstance(error,OSError) else 'configuration, prerequisite or Docker operation failed'
        print(f'Workflow/evidence failure: {category}; retained evidence: {run}', file=sys.stderr)
        return code if code in (1,2,3) else 3
    finally:
        cleanup = time.monotonic()
        if validator_launched:
            found = subprocess.run(['docker', 'inspect', name+'-validate'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
            if found.returncode == 0:
                try:
                    docker('rm', '-f', name+'-validate', timeout=10, stdout=subprocess.DEVNULL)
                except subprocess.SubprocessError:
                    print(f'Cleanup incomplete: {name}-validate', file=sys.stderr)
                    raise CleanupFailure(code if code in (1,2,3) else 3)
        if launched:
            try:
                docker('stop', '-t', '6', name, timeout=10, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                docker('rm', name, stdout=subprocess.DEVNULL)
            except subprocess.SubprocessError:
                print(f'Cleanup incomplete: {name}', file=sys.stderr)
                raise CleanupFailure(code if code in (1,2,3) else 3)
        print(f'Wrapper container cleanup: {time.monotonic()-cleanup:.3f}s', flush=True)
        for sig, handler in old.items():
            signal.signal(sig, handler)

def main():
    completion = {"cancelled": False}
    result = execute(completion)
    # Cancellation can arrive during finally/owned cleanup. Select the public
    # result only after that barrier, without replacing a stronger nonzero exit.
    return 1 if result == 0 and completion["cancelled"] else result

class CleanupFailure(Exception):
    pass

if __name__ == '__main__':
    try:
        result = main()
        print(f"Workflow exit: {result}", flush=True)
        sys.exit(result)
    except (OSError, ValueError, subprocess.SubprocessError):
        print("Workflow preparation or cleanup failed; no success claimed", file=sys.stderr)
        sys.exit(3)
    except CleanupFailure as failure:
        print(f"Workflow exit: {failure.args[0]}", flush=True)
        sys.exit(failure.args[0])
