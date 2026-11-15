#!/usr/bin/env python3
"""Retained Docker logs; capture never waits for the upload transport."""
import collections
import datetime
import hashlib
import hmac
import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.request
import urllib.error

MAX_LINE = 1 << 20

def parse_timestamp(timestamp):
    match = re.fullmatch(r'([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})'
                         r'(?:\.([0-9]{1,9}))?(Z|[+-][0-9]{2}:[0-9]{2})', timestamp)
    if not match: raise ValueError('Invalid Docker log timestamp or missing timezone')
    base, fraction, offset = match.groups()
    if offset == 'Z': offset = '+00:00'
    if int(offset[1:3]) > 23 or int(offset[4:6]) > 59:
        raise ValueError('Invalid Docker log timestamp timezone')
    fraction = fraction or ''
    # Python 3.10 accepts only three or six fractional digits. Normalize only
    # its datetime input; records and overlap proofs retain the original text.
    parsed = datetime.datetime.fromisoformat(base + '.' + (fraction + '000000')[:6] + offset)
    return parsed, int((fraction + '000000000')[:9])

def timestamp_key(timestamp):
    parsed, nanos = parse_timestamp(timestamp)
    return int(parsed.replace(microsecond=0).timestamp()), nanos

def overlap_digest(previous, timestamp, line):
    return hashlib.sha256(bytes.fromhex(previous) + timestamp.encode() + b'\x00' + line.encode()).hexdigest()

class Spool:
    def __init__(self, path, max_bytes=8 << 20):
        self.path, self.max_bytes = path, max_bytes
        self.lock = threading.Lock()
        self.rows = collections.deque()
        self.size, self.next, self.complete, self.error = 0, 1, False, ''
        self.replay_boundary = None
        self.checkpoint_timestamp, self.checkpoint_count, self.checkpoint_hash = None, 0, ''
        self.prefix_timestamp, self.prefix_count, self.prefix_hash = None, 0, ''
        if os.path.exists(path):
            with open(path) as f:
                state = json.load(f)
            self.rows.extend(state['records'])
            self.next, self.complete, self.error = state['next'], state['complete'], state['error']
            self.size = sum(self.cost(r) for r in self.rows)
            self.checkpoint_timestamp = state.get('checkpointTimestamp')
            self.checkpoint_count = state.get('checkpointCount', 0)
            self.checkpoint_hash = state.get('checkpointHash', '')
            if 'prefixCount' in state:
                self.prefix_timestamp = state.get('prefixTimestamp')
                self.prefix_count, self.prefix_hash = state['prefixCount'], state.get('prefixHash', '')
            elif self.next == len(self.rows) + 1 and all(
                    r['sequence'] == i for i, r in enumerate(self.rows, 1)):
                # Older helpers can upgrade only when their entire prefix remains.
                for r in self.rows:
                    self.extend_prefix(r['timestamp'], r['line'])
            else:
                self.prefix_count = None
            # A restarted live capture must finish a new finite Docker replay
            # before announcing that all persisted and uncaptured history is ready.
            if self.complete or self.error:
                self.replay_boundary = state.get('replayBoundary', self.next - 1)
    @staticmethod
    def cost(r): return len(r['line'].encode()) + 128
    def extend_prefix(self, timestamp, line):
        if self.prefix_count is None: return
        if self.prefix_count == 0: self.prefix_timestamp = timestamp
        self.prefix_count += 1
        self.prefix_hash = overlap_digest(self.prefix_hash, timestamp, line)
    def save(self):
        with open(self.path + '.tmp', 'w') as f:
            json.dump(dict(records=list(self.rows), next=self.next, complete=self.complete, error=self.error,
                           replayBoundary=self.replay_boundary, checkpointTimestamp=self.checkpoint_timestamp,
                           checkpointCount=self.checkpoint_count, checkpointHash=self.checkpoint_hash,
                           prefixTimestamp=self.prefix_timestamp, prefixCount=self.prefix_count,
                           prefixHash=self.prefix_hash), f)
        os.replace(self.path + '.tmp', self.path)
    def append(self, timestamp, line):
        with self.lock:
            r = dict(sequence=self.next, timestamp=timestamp, line=line)
            self.next += 1
            # Docker preserves record order but permits nonmonotonic timestamps.
            # Resume at the highest timestamp and skip its entire captured suffix.
            if self.checkpoint_timestamp is None or timestamp_key(timestamp) > timestamp_key(self.checkpoint_timestamp):
                self.checkpoint_timestamp, self.checkpoint_count, self.checkpoint_hash = timestamp, 0, ''
            self.checkpoint_count += 1
            self.checkpoint_hash = overlap_digest(self.checkpoint_hash, timestamp, line)
            self.extend_prefix(timestamp, line)
            self.rows.append(r);self.size += self.cost(r)
            while self.rows and (self.size > self.max_bytes or len(self.rows) > 5000):
                self.size -= self.cost(self.rows.popleft())
            self.save()
    def finish(self, error=''):
        with self.lock:
            self.error, self.complete = error[:1024], not bool(error)
            if self.complete and self.replay_boundary is None:
                self.replay_boundary = self.next - 1
            self.save()
    def mark_replay_complete(self):
        with self.lock:
            self.replay_boundary = self.next - 1
            self.save()
    def checkpoint(self):
        with self.lock:
            return self.checkpoint_timestamp, self.checkpoint_count
    def checkpoint_proof(self):
        with self.lock:
            return self.checkpoint_timestamp, self.checkpoint_count, self.checkpoint_hash
    def prefix_proof(self):
        with self.lock:
            if self.prefix_count is None:
                raise ValueError('Docker history proof unavailable after helper upgrade: retained prefix missing')
            return self.prefix_timestamp, self.prefix_count, self.prefix_hash
    def snapshot(self, with_boundary=False):
        with self.lock:
            result = (list(self.rows), (self.rows[0]['sequence'] if self.rows else self.next), self.complete, self.error)
            return result + (self.replay_boundary,) if with_boundary else result

def read_docker(spool, args, checkpoint, skip, expected_hash, timeout=None):
    skipped_hash = ''
    first_overlap = True
    process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    timer = threading.Timer(timeout, process.kill) if timeout else None
    if timer: timer.start()
    try:
        while True:
            raw = process.stdout.readline(MAX_LINE + 130)
            if not raw: break
            if len(raw) > MAX_LINE + 100: raise ValueError('Docker log record exceeds line limit')
            text = raw.decode('utf-8', errors='replace').rstrip('\n')
            ts, sep, line = text.partition(' ')
            if not sep: raise ValueError('Docker log missing timestamp')
            parse_timestamp(ts)
            if len(line.encode()) > MAX_LINE: raise ValueError('Docker log record exceeds line limit')
            if len(json.dumps(line).encode()) > (2 << 20) - 4096:
                raise ValueError('Docker log encoding exceeds upload frame limit')
            if skip:
                if first_overlap and timestamp_key(ts) != timestamp_key(checkpoint):
                    raise ValueError('Docker retained history lost at restart checkpoint')
                first_overlap = False
                skipped_hash = overlap_digest(skipped_hash, ts, line)
                skip -= 1
                if skip == 0 and skipped_hash != expected_hash:
                    raise ValueError('Docker retained history lost in restart overlap')
                continue
            spool.append(ts, line)
        if process.wait() != 0: raise ValueError('docker logs transport failed or replay timed out')
        if skip: raise ValueError('Docker retained history lost at restart checkpoint')
    finally:
        if timer: timer.cancel()
        if process.poll() is None:
            process.kill();process.wait()
        process.stdout.close()

def docker_supports_suffix_replay(docker):
    try:
        result = subprocess.run([docker, 'version', '--format', '{{.Server.Version}}'],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                timeout=5, check=True, text=True)
        version = re.fullmatch(r'([0-9]+)\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z._+-]+)?', result.stdout.strip())
        return bool(version and int(version.group(1)) >= 24)
    except (OSError, subprocess.SubprocessError, UnicodeError):
        return False

def capture(spool, container, docker='docker', replay_timeout=60):
    try:
        _, _, complete, error = spool.snapshot()
        if complete or error: return
        suffix_replay = docker_supports_suffix_replay(docker)
        # Older Docker filters every record by --since, dropping backward-time
        # records. Full replay instead verifies a constant-size durable prefix.
        get_proof = spool.checkpoint_proof if suffix_replay else spool.prefix_proof
        cutoff = datetime.datetime.now(datetime.timezone.utc).isoformat()
        checkpoint, count, proof = get_proof()
        args = [docker, 'logs', '--timestamps', '--until', cutoff]
        if suffix_replay and checkpoint: args += ['--since', checkpoint]
        # Finite Docker history has an explicit EOF. Until it arrives, an empty
        # or partial VM spool is never advertised as historical replay complete.
        read_docker(spool, args + [container], checkpoint, count, proof, replay_timeout)
        spool.mark_replay_complete()
        checkpoint, count, proof = get_proof()
        # Replay the highest timestamp's captured suffix with a fixed-size
        # digest proof; this also handles out-of-order stdout/stderr timestamps.
        args = [docker, 'logs', '--timestamps', '--follow']
        if suffix_replay: args += ['--since', checkpoint or cutoff]
        read_docker(spool, args + [container], checkpoint, count, proof)
        spool.finish()
    except Exception as e:
        spool.finish('capture: ' + str(e))

class Sender:
    def __init__(self, spool, url, workload, generation, key):
        self.spool, self.url, self.workload, self.generation, self.key = spool, url, workload, generation, key
        self.epoch, self.cursor = '', 1
        self.replay_until = None
    def step(self):
        rows, first, complete, error, boundary = self.spool.snapshot(with_boundary=True)
        if self.replay_until is None and boundary is not None:
            self.replay_until = max(boundary, rows[-1]['sequence'] if rows else first - 1)
        selected, size = [], 0
        for r in rows:
            if r['sequence'] < self.cursor: continue
            cost = len(json.dumps(r).encode())
            if selected and size + cost > (1 << 20): break
            selected.append(r);size += cost
        last = not rows or not selected or selected[-1]['sequence'] == rows[-1]['sequence']
        batch = dict(workloadID=self.workload, generation=self.generation, epoch=self.epoch,
                     firstSequence=first, records=selected,
                     replayComplete=(self.replay_until is not None and
                                     (selected[-1]['sequence'] if selected else self.cursor - 1) >= self.replay_until),
                     complete=complete and last, error=error if last else '')
        body = json.dumps(batch, separators=(',', ':'), ensure_ascii=False).encode()
        ts = str(int(time.time()))
        sig = 'v1=' + hmac.new(self.key.encode(), ts.encode() + b'.' + body, hashlib.sha256).hexdigest()
        req = urllib.request.Request(self.url, data=body, headers={
            'Content-Type': 'application/json', 'X-NeoCloud-Runner-Timestamp': ts,
            'X-NeoCloud-Runner-Signature': sig})
        try:
            with urllib.request.urlopen(req, timeout=10) as response:
                ack = json.loads(response.read(4097))
            if not isinstance(ack['epoch'], str) or not ack['epoch']: return False
            if self.epoch != ack['epoch'] or ack.get('replayRequired'):
                self.epoch, self.cursor = ack['epoch'], first
                self.replay_until = None
            else:
                self.cursor = int(ack['nextSequence'])
            return True
        except urllib.error.HTTPError as e:
            e.close()
            return False
        except Exception:
            return False

def main():
    # Only a workload-scoped key is present in this private file.
    with open(sys.argv[1]) as f: config = json.load(f)
    os.makedirs(config['directory'], mode=0o700, exist_ok=True)
    spool = Spool(config['directory'] + '/spool.json')
    threading.Thread(target=capture, args=(spool, config['container']), daemon=True).start()
    sender = Sender(spool, config['url'], config['workloadID'], config['generation'], config['key'])
    finished_at = None
    while True:
        sender.step()
        if spool.snapshot()[2] or spool.snapshot()[3]:
            if finished_at is None: finished_at = time.monotonic()
            if time.monotonic() - finished_at > 86400: return
        time.sleep(2)

if __name__ == '__main__': main()
