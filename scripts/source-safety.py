"""Bounded text/source checks shared by the Git publication audit.

Pattern matching is not a proof of absence of arbitrary secrets.
"""
import base64, hashlib, ipaddress, os, re, stat
from pathlib import Path, PurePosixPath
MAX_FILE=8*1024*1024
class Rejected(Exception):pass
def digest(b):return hashlib.sha256(b).hexdigest()
def safe_path(p):
 q=PurePosixPath(p)
 return bool(p) and str(q)==p and not q.is_absolute() and not any(x in ('..','.git','.agents','evidence','state','runs','sessions','__pycache__','node_modules') for x in q.parts) and not q.name.startswith('.env') and q.suffix.lower() not in ('.pem','.key','.crt','.bin','.exe','.zip','.gz','.tar','.log')
def read(root,p):
 if not safe_path(p):raise Rejected('forbidden selected path')
 descriptors=[];edges=[]
 try:
  parent=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW);descriptors.append(parent)
  for part in PurePosixPath(p).parts[:-1]:
   child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=parent)
   descriptors.append(child);edges.append((parent,part,os.fstat(child)));parent=child
  name=PurePosixPath(p).name
  initial=os.stat(name,dir_fd=parent,follow_symlinks=False)
  if not stat.S_ISREG(initial.st_mode):raise Rejected('unsafe source type/size')
  fd=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=parent);descriptors.append(fd)
  a=os.fstat(fd);edges.append((parent,name,a))
  if not stat.S_ISREG(a.st_mode) or a.st_nlink!=1 or a.st_size>MAX_FILE:raise Rejected('unsafe source type/size')
  if (initial.st_dev,initial.st_ino)!=(a.st_dev,a.st_ino):raise Rejected('source changed')
  with os.fdopen(fd,'rb',closefd=False) as f:b=f.read(MAX_FILE+1)
  z=os.fstat(fd)
  if len(b)!=a.st_size or (a.st_ino,a.st_size,a.st_mtime_ns)!=(z.st_ino,z.st_size,z.st_mtime_ns):raise Rejected('source changed')
  for directory,entry,opened in edges:
   current=os.stat(entry,dir_fd=directory,follow_symlinks=False)
   if (current.st_dev,current.st_ino,current.st_mode)!=(opened.st_dev,opened.st_ino,opened.st_mode):raise Rejected('source changed')
 finally:
  for descriptor in reversed(descriptors):os.close(descriptor)
 try:b.decode('utf-8')
 except UnicodeError:raise Rejected('non-text selected source')
 if b'\0' in b:raise Rejected('binary selected source')
 return b,0o755 if a.st_mode&0o111 else 0o644
IP=re.compile(r'(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])')
PATTERNS=[('private-key',re.compile(r'-----BEGIN (?:RSA |EC |OPENSSH |ENCRYPTED )?PRIVATE KEY-----')),('token',re.compile(r'(?:AKIA|ASIA)[A-Z0-9]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{15,}')),('credential',re.compile(r'(?i)["\x27]?(?:password|passwd|api[_-]?key|client[_-]?secret|access[_-]?token|token|secret[_-]?key)["\x27]?\s*[:=]\s*["\x27]([^"\x27\r\n]{4,})["\x27]')),('local-path',re.compile(r'/(?:home|Users)/[A-Za-z0-9_.-]+/'))]
PATTERNS += [
 ('credential',re.compile(r"\b[A-Z][A-Z0-9_]*(?:TOKEN|PASSWORD|SECRET|ACCESS_KEY|API_KEY)[A-Z0-9_]*\s*=\s*[\"']?[A-Za-z0-9/+=._-]{8,}")),
 ('credential',re.compile(r"(?i)(?:^|[ {,])(?:password|passwd|apiKey|clientSecret|accessToken|secretAccessKey|token)\s*:\s*[A-Za-z0-9/+=._-]{4,}")),
 ('credential-url',re.compile(r"https?://[^\s/:@]+:[^\s/@]+@")),
]
def classes(text):
 out=set()
 for label,pattern in PATTERNS:
  if pattern.search(text):out.add(label)
 for raw in IP.findall(text):
  try:a=ipaddress.IPv4Address(raw)
  except ValueError:continue
  if a.is_loopback or a.is_unspecified:continue
  if any(a in ipaddress.ip_network(n) for n in ('192.0.2.0/24','198.51.100.0/24','203.0.113.0/24')):out.add('documentation-address')
  else:out.add('address')
 return out

def findings(data):
 out=[]
 for n,line in enumerate(data.decode().splitlines(),1):
  labels=classes(line)
  # Bounded one-level decoding catches encoded recognized credentials/addresses, not arbitrary opaque secrets.
  for value in re.findall(r'(?<![A-Za-z0-9+/])[A-Za-z0-9+/]{16,}={0,2}(?![A-Za-z0-9+/])',line):
   if len(value)>4096:continue
   try:decoded=base64.b64decode(value,validate=True).decode('utf-8')
   except (ValueError,UnicodeError):continue
   labels|={'encoded-'+x for x in classes(decoded)}
  for label in sorted(labels):out.append({'line':n,'class':label,'lineSHA256':digest(line.encode())})
 return out

