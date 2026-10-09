#!/usr/bin/env python3
"""Controlled 50-source integration/load test. Never contacts public DNS."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import socket
import socketserver
import struct
import subprocess
import tempfile
import threading
import time


def exact(sock, n):
    out=bytearray()
    while len(out)<n:
        block=sock.recv(n-len(out))
        if not block: raise EOFError
        out.extend(block)
    return bytes(out)


def answer(wire, ip):
    end=12
    while wire[end]: end += wire[end]+1
    end += 5
    return struct.pack('!HHHHHH',struct.unpack('!H',wire[:2])[0],0x8180,1,1,0,0)+wire[12:end]+b'\xc0\x0c'+struct.pack('!HHIH',1,1,120,4)+socket.inet_aton(ip)


class UDP(socketserver.ThreadingUDPServer):
    daemon_threads=True
class TCP(socketserver.ThreadingTCPServer):
    daemon_threads=True
    allow_reuse_address=True
    request_queue_size=256
class QueryFailure(RuntimeError): pass

class State:
    def __init__(self): self.lock=threading.Lock();self.direct=0;self.proxy=0;self.probes=0;self.proxy_probes=0
    def increment(self,key):
        with self.lock: setattr(self,key,getattr(self,key)+1)
    def counts(self):
        with self.lock: return dict(direct=self.direct,proxy=self.proxy,probes=self.probes,proxy_probes=self.proxy_probes)


def run(binary, expect_shared, count):
    state=State()
    class Direct(socketserver.BaseRequestHandler):
        def handle(self):
            wire,sock=self.request;state.increment('direct');sock.sendto(answer(wire,'127.0.0.1'),self.client_address)
    class ProxyDNS(socketserver.BaseRequestHandler):
        def handle(self):
            self.request.settimeout(5)
            try:
                while True:
                    n=struct.unpack('!H',exact(self.request,2))[0];wire=exact(self.request,n)
                    state.increment('proxy');reply=answer(wire,'127.0.0.2')
                    self.request.sendall(struct.pack('!H',len(reply))+reply)
            except (EOFError,OSError): pass
    class Probe(socketserver.BaseRequestHandler):
        def handle(self):
            state.increment('probes')
            if self.request.getsockname()[0]=='127.0.0.2': state.increment('proxy_probes')
    servers=[]
    def serve(kind,handler,host='127.0.0.1'):
        s=kind((host,0),handler);servers.append(s)
        threading.Thread(target=s.serve_forever,daemon=True).start();return s.server_address[1]
    dp=serve(UDP,Direct);pp=serve(TCP,ProxyDNS);probe_port=serve(TCP,Probe,'0.0.0.0')
    class Socks(socketserver.BaseRequestHandler):
        def handle(self):
            s=self.request;s.settimeout(5)
            try:
                version,n=exact(s,2);assert version==5;exact(s,n);s.sendall(b'\x05\x00')
                version,command,_,kind=exact(s,4);assert version==5 and command==1
                if kind==1: host=socket.inet_ntoa(exact(s,4))
                elif kind==3: host=exact(s,exact(s,1)[0]).decode()
                else: raise AssertionError('unexpected resolver address')
                port=struct.unpack('!H',exact(s,2))[0]
                assert host=='127.0.0.1' and port==pp, (host,port)
                with socket.create_connection((host,port),timeout=5) as remote:
                    s.sendall(b'\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x00')
                    while True:
                        size=exact(s,2);wire=exact(s,struct.unpack('!H',size)[0]);remote.sendall(size+wire)
                        size=exact(remote,2);s.sendall(size+exact(remote,struct.unpack('!H',size)[0]))
            except (EOFError,OSError): pass
    sp=serve(TCP,Socks)
    with socket.socket() as reserve:
        reserve.bind(('127.0.0.1',0));dnsport=reserve.getsockname()[1]
    process=None
    try:
        with tempfile.TemporaryDirectory(prefix='mihomo-dns-perf-') as directory:
            root=Path(directory)
            config=f'''mode: rule
log-level: silent
dns-rule-routing: true
find-process-mode: off
proxies:
  - name: TestProxy
    type: socks5
    server: 127.0.0.1
    port: {sp}
    udp: false
rules:
  - DOMAIN-SUFFIX,denied.test,REJECT
  - SRC-IP-CIDR,127.0.0.3/32,DIRECT
  - DOMAIN-SUFFIX,proxy.test,TestProxy
  - MATCH,DIRECT
dns:
  enable: true
  listen: 127.0.0.1:{dnsport}
  enhanced-mode: redir-host
  ipv6: false
  default-nameserver: [127.0.0.1:{dp}]
  proxy-server-nameserver: [127.0.0.1:{dp}]
  direct-nameserver: [127.0.0.1:{dp}]
  nameserver: [tcp://127.0.0.1:{pp}]
  cache-algorithm: arc
  cache-max-size: 65536
  prefetch-domain: false
  serve-expired: false
  speed-check-mode: [tcp:{probe_port}]
  speed-check-timeout: 1000
  speed-check-concurrency: 16
'''
            (root/'config.yaml').write_text(config)
            log=open(root/'log','w+')
            process=subprocess.Popen([str(Path(binary).resolve()),'-d',str(root),'-f',str(root/'config.yaml')],stdout=log,stderr=subprocess.STDOUT)
            def query(source,name,want=None,refused=False):
                ident=time.monotonic_ns()&65535
                qname=b''.join(bytes([len(label)])+label.encode() for label in name.split('.'))+b'\x00'
                wire=struct.pack('!HHHHHH',ident,0x100,1,0,0,0)+qname+struct.pack('!HH',1,1)
                with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as s:
                    s.bind((source,0));s.settimeout(5)
                    started=time.perf_counter();s.sendto(wire,('127.0.0.1',dnsport));reply,_=s.recvfrom(65535)
                    elapsed=time.perf_counter()-started
                rid,flags=struct.unpack('!HH',reply[:4]);assert rid==ident
                if refused: assert flags&15==5,reply
                else:
                    if flags&15==2: raise QueryFailure('SERVFAIL')
                    assert flags&15==0,reply
                    assert reply[-4:]==socket.inet_aton(want), (source,name,reply)
                return elapsed
            for _ in range(60):
                if process.poll() is not None:
                    log.flush();log.seek(0);raise RuntimeError(log.read())
                try: query('127.0.0.2','ready.direct.test','127.0.0.1');break
                except (OSError,AssertionError): time.sleep(.1)
            else: raise RuntimeError('native DNS did not become ready')
            # Same domain, different source rules: preserve SRC priority and pool.
            query('127.0.0.3','source.proxy.test','127.0.0.1')
            before=state.counts()
            query('127.0.0.4','source.proxy.test','127.0.0.2')
            assert state.counts()['proxy_probes']==0, 'proxy answers were locally probed'
            before=state.counts()
            query('127.0.0.3','blocked.denied.test',refused=True)
            assert all(state.counts()[key]==before[key] for key in ('direct','proxy')), 'REJECT queried an upstream'
            sources=[f'127.0.0.{i}' for i in range(10,60)]
            for source in sources:
                query(source,'hot.direct.test','127.0.0.1')
                query(source,'hot.proxy.test','127.0.0.2')
            before=state.counts()
            failures=[]
            failures_lock=threading.Lock()
            def client(source):
                latencies=[]
                for i in range(count):
                    remote=i%2==1
                    try:
                        latencies.append(query(source,'hot.proxy.test' if remote else 'hot.direct.test','127.0.0.2' if remote else '127.0.0.1'))
                    except (QueryFailure,TimeoutError) as error:
                        if expect_shared: raise
                        with failures_lock: failures.append(type(error).__name__)
                return latencies
            start=time.perf_counter()
            with ThreadPoolExecutor(max_workers=50) as pool:
                latencies=[v for result in pool.map(client,sources) for v in result]
            elapsed=time.perf_counter()-start;after=state.counts()
            extra=(after['direct']-before['direct'])+(after['proxy']-before['proxy'])
            if expect_shared: assert extra==0, f'warmed answer cache missed when only source ports changed: {extra}'
            # A separate simultaneous cold phase exercises shared IP probes.
            def cold(source):
                for i in range(4):
                    try: query(source,f'cold-{source.rsplit(".",1)[1]}-{i}.direct.test','127.0.0.1')
                    except (QueryFailure,TimeoutError) as error:
                        if expect_shared: raise
                        with failures_lock: failures.append('cold:'+type(error).__name__)
            cold_before=state.counts()
            with ThreadPoolExecutor(max_workers=50) as pool:list(pool.map(cold,sources))
            cold_after=state.counts()
            assert state.counts()['proxy_probes']==0, 'proxy answer IP was probed'
            if not latencies: raise RuntimeError('all controlled queries failed')
            latencies.sort()
            result=dict(sources=50,requests=50*count,successful_warm_queries=len(latencies),failed_queries=len(failures),seconds=round(elapsed,3),qps=round(len(latencies)/elapsed),
                        p50_ms=round(latencies[int(.50*(len(latencies)-1))]*1000,3),
                        p95_ms=round(latencies[int(.95*(len(latencies)-1))]*1000,3),
                        p99_ms=round(latencies[int(.99*(len(latencies)-1))]*1000,3),
                        warm_upstream_queries=extra,cold_queries=200,cold_probe_connections=cold_after['probes']-cold_before['probes'])
            print(('PASS' if not failures else 'BASELINE WITH FAILURES')+' 50-source controlled DNS load; SRC / pool / REJECT / proxy-no-probe assertions passed')
            print(json.dumps(result,sort_keys=True))
    finally:
        if process is not None:
            process.terminate()
            try:process.wait(timeout=5)
            except subprocess.TimeoutExpired:process.kill();process.wait()
        for server in servers:server.shutdown();server.server_close()


if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('binary');p.add_argument('--expect-shared',action='store_true');p.add_argument('--per-client',type=int,default=20)
    a=p.parse_args();run(a.binary,a.expect_shared,a.per_client)
