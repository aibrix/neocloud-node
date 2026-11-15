import importlib.util
import json
import os
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

class ShipperTest(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location('logship', Path(__file__).with_name('logship.py'))
        self.m = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.m)
        self.tmp = tempfile.TemporaryDirectory()
    def tearDown(self):
        self.tmp.cleanup()
    def spool(self, **kw):
        return self.m.Spool(self.tmp.name + '/spool.json', **kw)
    def legacy_docker(self, rows, version='20.10.8'):
        docker, calls = self.tmp.name + '/docker', self.tmp.name + '/calls'
        with open(docker, 'w') as f:
            f.write('#!/usr/bin/env python3\nimport sys,json\n'
                    'if sys.argv[1:2] == ["version"]: print(' + repr(version) + ');sys.exit(0)\n'
                    'with open(' + repr(calls) + ',"a") as f: f.write(json.dumps(sys.argv)+"\\n")\n'
                    'rows=' + repr(rows) + '\n'
                    'if "--follow" in sys.argv: rows.append(("2026-01-01T00:00:00Z","live-backward"))\n'
                    'since=sys.argv[sys.argv.index("--since")+1] if "--since" in sys.argv else ""\n'
                    'for ts,line in rows:\n'
                    ' if ts>=since: print(ts+" "+line)\n')
        os.chmod(docker, 0o755)
        return docker, calls
    def test_legacy_or_unknown_version_replays_full_prefix_and_new_backward_records(self):
        rows = [('2026-01-01T00:00:01Z', 'a'), ('2026-01-01T00:00:02Z', 'b'),
                ('2026-01-01T00:00:01Z', 'c')]
        for version in ['20.10.8', 'unknown', '', '24', '24.0.broken']:
            with self.subTest(version=version):
                path = self.tmp.name + '/spool-' + version + '.json'
                s = self.m.Spool(path, max_bytes=160)
                for ts,line in rows: s.append(ts,line)
                restored = self.m.Spool(path, max_bytes=160)
                docker,calls = self.legacy_docker(rows + [('2026-01-01T00:00:00Z','new-backward')], version)
                self.m.capture(restored,'container',docker=docker)
                kept,_,complete,error = restored.snapshot()
                self.assertEqual(error,'');self.assertTrue(complete)
                self.assertEqual([(r['sequence'],r['line']) for r in kept], [(5,'live-backward')])
                with open(calls) as f:
                    for args in map(json.loads,f): self.assertNotIn('--since',args)
    def test_legacy_replay_rejects_missing_or_changed_prefix(self):
        stamp='2026-01-01T00:00:01Z'
        for rows in [[], [(stamp,'replacement')], [(stamp,'first')]]:
            with self.subTest(rows=rows):
                path=self.tmp.name+'/loss-'+str(len(rows))+str(rows==[(stamp,'first')])+'.json'
                s=self.m.Spool(path,max_bytes=160)
                for line in ['first','second']: s.append(stamp,line)
                docker,_=self.legacy_docker(rows)
                restored=self.m.Spool(path,max_bytes=160)
                self.m.capture(restored,'container',docker=docker)
                self.assertIn('history lost',restored.snapshot()[3])
                self.assertFalse(restored.snapshot()[2])
                self.assertEqual(restored.snapshot()[0],s.snapshot()[0])
    def test_server_version_failure_or_timeout_uses_full_replay(self):
        from unittest import mock
        stamp = '2026-01-01T00:00:01Z'
        for failure in [subprocess.TimeoutExpired('docker version',5),
                        subprocess.CalledProcessError(1,'docker version'), OSError('unavailable'),
                        UnicodeDecodeError('utf8',b'\xff',0,1,'invalid version output')]:
            with self.subTest(failure=type(failure).__name__):
                path = self.tmp.name + '/version-' + type(failure).__name__ + '.json'
                s = self.m.Spool(path);s.append(stamp,'first')
                docker,calls = self.legacy_docker([(stamp,'first')])
                with mock.patch.object(self.m.subprocess,'run',side_effect=failure):
                    self.m.capture(s,'container',docker=docker)
                self.assertEqual(s.snapshot()[3],'')
                self.assertEqual([r['line'] for r in s.snapshot()[0]],['first','live-backward'])
                with open(calls) as f:
                    for args in map(json.loads,f): self.assertNotIn('--since',args)
    def test_legacy_upgrade_reconstructs_only_fully_retained_prefix(self):
        stamp='2026-01-01T00:00:01Z'
        for limit in [10000,160]:
            with self.subTest(limit=limit):
                path=self.tmp.name+'/upgrade-'+str(limit)+'.json'
                s=self.m.Spool(path,max_bytes=limit)
                for line in ['first','second']:s.append(stamp,line)
                with open(path) as f:state=json.load(f)
                for key in list(state):
                    if key.startswith('prefix'):del state[key]
                with open(path,'w') as f:json.dump(state,f)
                docker,_=self.legacy_docker([(stamp,'first'),(stamp,'second')])
                restored=self.m.Spool(path,max_bytes=limit)
                self.m.capture(restored,'container',docker=docker)
                if limit==160:
                    self.assertIn('upgrade',restored.snapshot()[3])
                    self.assertEqual(restored.snapshot()[0],s.snapshot()[0])
                else:
                    self.assertEqual(restored.snapshot()[3],'')
                    self.assertEqual([r['line'] for r in restored.snapshot()[0]],['first','second','live-backward'])
    def test_rfc3339_fraction_widths_and_offsets_keep_nanosecond_order(self):
        for width in range(10):
            fraction = '.' + '123456789'[:width] if width else ''
            nanos = [0, 100000000, 120000000, 123000000, 123400000,
                     123450000, 123456000, 123456700, 123456780, 123456789][width]
            for stamp in ['1970-01-01T00:00:00%sZ' % fraction,
                          '1970-01-01T01:30:00%s+01:30' % fraction,
                          '1969-12-31T22:30:00%s-01:30' % fraction]:
                with self.subTest(stamp=stamp):
                    self.assertEqual(self.m.timestamp_key(stamp), (0, nanos))
        self.assertLess(self.m.timestamp_key('2026-11-14T14:17:06.499884254Z'),
                        self.m.timestamp_key('2026-11-14T14:17:06.499884255Z'))
    def test_timestamp_rejects_missing_or_malformed_timezone_and_fraction(self):
        for stamp in ['2026-11-14T14:17:06', '2026-11-14T14:17:06.1',
                      '2026-11-14T14:17:06+24:00', '2026-11-14T14:17:06+00:60',
                      '2026-11-14T14:17:06+0000', '2026-11-14T14:17:06+00',
                      '2026-11-14T14:17:06+00:00:01',
                      '2026-11-14T14:17:06.Z', '2026-11-14T14:17:06.1234567890Z',
                      '2026-02-30T14:17:06Z']:
            with self.subTest(stamp=stamp):
                with self.assertRaises(ValueError): self.m.timestamp_key(stamp)
    def test_nanosecond_capture_restart_preserves_original_records_and_checkpoint(self):
        earlier = '2026-11-14T14:17:06.499884254+00:00'
        later = '2026-11-14T14:17:06.499884255+00:00'
        s = self.spool()
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f:
            f.write('#!/bin/sh\nif [ "$1" = version ]; then echo 24.0.9; exit 0; fi\nprintf "' + earlier + ' first\\n' + later + ' second\\n"\n')
        os.chmod(docker, 0o755)
        self.m.read_docker(s, [docker], None, 0, '')
        self.assertEqual([r['timestamp'] for r in s.snapshot()[0]], [earlier, later])
        self.assertEqual(s.checkpoint(), (later, 1))
        restored = self.spool()
        with open(docker, 'w') as f:
            f.write('#!/bin/sh\nif [ "$1" = version ]; then echo 24.0.9; exit 0; fi\nprintf "' + later + ' second\\n' + later + ' third\\n"\n')
        self.m.read_docker(restored, [docker], *restored.checkpoint_proof())
        self.assertEqual([(r['timestamp'], r['line']) for r in restored.snapshot()[0]],
                         [(earlier, 'first'), (later, 'second'), (later, 'third')])
        # A timestamp changed only in the nanoseconds must fail the overlap proof.
        changed = self.spool()
        with open(docker, 'w') as f:
            f.write('#!/bin/sh\nif [ "$1" = version ]; then echo 24.0.9; exit 0; fi\nprintf "' + later + ' second\\n' + earlier + ' third\\n"\n')
        with self.assertRaisesRegex(ValueError, 'history lost in restart overlap'):
            self.m.read_docker(changed, [docker], *changed.checkpoint_proof())
    def test_capture_real_process_timestamps_and_completion(self):
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f:
            f.write('#!/bin/sh\nif [ "$1" = version ]; then echo 24.0.9; exit 0; fi\ncase " $* " in *--follow*) printf "2026-01-01T00:00:01Z world\\n";; *) printf "2026-01-01T00:00:00.123456789Z hello\\n2026-01-01T00:00:01Z world\\n";; esac\n')
        os.chmod(docker, 0o755)
        s = self.spool()
        self.m.capture(s, 'container', docker=docker)
        rows, first, complete, error = s.snapshot()
        self.assertEqual([r['line'] for r in rows], ['hello', 'world'])
        self.assertEqual(rows[0]['timestamp'], '2026-01-01T00:00:00.123456789Z')
        self.assertTrue(complete)
        self.assertEqual(error, '')
        restored = self.spool()
        self.assertEqual(restored.snapshot(), s.snapshot())
    def test_retention_keeps_acknowledged_records_and_bounds(self):
        s = self.spool(max_bytes=300)
        for i in range(10):
            s.append('2026-01-01T00:00:00Z', str(i) * 30)
        rows, first, _, _ = s.snapshot()
        self.assertGreater(first, 1)
        self.assertLessEqual(s.size, 300)
        self.assertEqual(rows[-1]['sequence'], 10)
    def test_http_failure_retry_epoch_reset_replay_and_auth(self):
        import hashlib, hmac
        s = self.spool()
        s.append('2026-01-01T00:00:00Z', 'kept')
        s.finish()
        seen = []
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args): pass
            def do_POST(inner):
                body = inner.rfile.read(int(inner.headers['Content-Length']))
                ts = inner.headers['X-NeoCloud-Runner-Timestamp']
                sig = 'v1=' + hmac.new(b'key', ts.encode() + b'.' + body, hashlib.sha256).hexdigest()
                self.assertEqual(inner.headers['X-NeoCloud-Runner-Signature'], sig)
                b = json.loads(body); seen.append(b)
                if len(seen) == 1:
                    inner.send_response(503); inner.end_headers(); return
                epoch = 'second' if len(seen) >= 4 else 'first'
                reply = dict(epoch=epoch, nextSequence=2, replayRequired=b['epoch'] != epoch)
                inner.send_response(200);inner.end_headers();inner.wfile.write(json.dumps(reply).encode())
        server = HTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True);thread.start()
        sender = self.m.Sender(s, 'http://127.0.0.1:%d' % server.server_port, 'ns/uid', 'generation', 'key')
        try:
            for _ in range(6): sender.step()
        finally:
            server.shutdown();server.server_close();thread.join()
        self.assertEqual([r['line'] for r in seen[2]['records']], ['kept'])
        self.assertEqual([r['line'] for r in seen[4]['records']], ['kept'])
        self.assertTrue(seen[4]['replayComplete'])
        self.assertTrue(seen[4]['complete'])
        self.assertEqual(len(s.snapshot()[0]), 1)
    def test_bad_docker_output_is_error_not_empty_success(self):
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f: f.write('#!/bin/sh\nif [ "$1" = version ]; then echo 24.0.9; exit 0; fi\necho malformed\n')
        os.chmod(docker, 0o755)
        s = self.spool();self.m.capture(s, 'container', docker=docker)
        self.assertIn('timestamp', s.snapshot()[3])
        self.assertFalse(s.snapshot()[2])
    def test_escaped_record_cannot_exceed_http_frame_bound(self):
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f:
            f.write('#!/usr/bin/env python3\nimport sys\nif sys.argv[1:2] == ["version"]: print("24.0.9");sys.exit(0)\nimport sys\nsys.stdout.write("2026-01-01T00:00:00Z " + chr(1) * 400000 + "\\n")\n')
        os.chmod(docker, 0o755)
        s = self.spool();self.m.capture(s, 'container', docker=docker)
        self.assertIn('encoding', s.snapshot()[3])
        self.assertEqual(s.snapshot()[0], [])
    def test_delayed_initial_history_cannot_finish_replay(self):
        import time
        from unittest import mock
        ready, release = self.tmp.name + '/ready', self.tmp.name + '/release'
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f:
            f.write('#!/usr/bin/env python3\nimport sys\nif sys.argv[1:2] == ["version"]: print("24.0.9");sys.exit(0)\nimport os,sys,time\n'
                    'ready,release=' + repr((ready, release)) + '\n'
                    'if not os.path.exists(ready):\n'
                    ' print("2026-01-01T00:00:00Z first",flush=True)\n'
                    ' open(ready,"w").close()\n'
                    ' while not os.path.exists(release): time.sleep(0.01)\n'
                    ' print("2026-01-01T00:00:01Z second",flush=True)\n'
                    'else:\n'
                    ' print("2026-01-01T00:00:01Z second",flush=True)\n'
                    ' print("2026-01-01T00:00:02Z live",flush=True)\n')
        os.chmod(docker, 0o755)
        s = self.spool()
        thread = threading.Thread(target=self.m.capture, args=(s, 'container', docker))
        thread.start()
        seen = []
        class Response:
            def __enter__(self): return self
            def __exit__(self, *args): pass
            def read(self, _):
                b = seen[-1]
                next_seq = b['records'][-1]['sequence'] + 1 if b['records'] else 1
                return json.dumps(dict(epoch='controller', nextSequence=next_seq,
                                       replayRequired=b['epoch'] != 'controller')).encode()
        def upload(req, **kw):
            seen.append(json.loads(req.data));return Response()
        sender = self.m.Sender(s, 'http://controller/logs', 'ns/uid', 'generation', 'key')
        try:
            deadline = time.monotonic() + 5
            while not os.path.exists(ready) and time.monotonic() < deadline: time.sleep(0.01)
            self.assertTrue(os.path.exists(ready), 'fake Docker never began replay')
            with mock.patch.object(self.m.urllib.request, 'urlopen', upload):
                sender.step();sender.step()
                self.assertFalse(any(b['replayComplete'] for b in seen),
                                 'partial Docker history falsely completed replay')
                open(release, 'w').close();thread.join(5)
                self.assertFalse(thread.is_alive())
                sender.step()
                self.assertTrue(seen[-1]['replayComplete'])
                self.assertEqual([r['line'] for r in s.snapshot()[0]], ['first', 'second', 'live'])
        finally:
            open(release, 'a').close();thread.join(5)
    def test_live_helper_restart_replays_checkpoint_after_spool_eviction(self):
        stamp = '2026-01-01T00:00:00Z'
        s = self.spool(max_bytes=160)
        for line in ['one', 'two', 'three']: s.append(stamp, line)
        s.mark_replay_complete()
        self.assertEqual(len(s.snapshot()[0]), 1)
        restored = self.spool(max_bytes=160)
        self.assertIsNone(restored.snapshot(with_boundary=True)[4],
                          'live helper restart trusted stale capture replay boundary')
        self.assertEqual(restored.checkpoint(), (stamp, 3))
        docker = self.tmp.name + '/docker'
        calls = self.tmp.name + '/calls'
        with open(docker, 'w') as f:
            f.write('#!/usr/bin/env python3\nimport sys\nif sys.argv[1:2] == ["version"]: print("24.0.9");sys.exit(0)\nimport sys,json\n'
                    'with open(' + repr(calls) + ',"a") as f: f.write(json.dumps(sys.argv)+"\\n")\n'
                    'lines=["one","two","three","four"]\n'
                    'if "--follow" in sys.argv: lines.append("five")\n'
                    'for line in lines: print(' + repr(stamp + ' ') + '+line)\n')
        os.chmod(docker, 0o755)
        self.m.capture(restored, 'container', docker=docker)
        rows, first, complete, error = restored.snapshot()
        self.assertEqual(error, '')
        self.assertTrue(complete)
        self.assertEqual([(r['sequence'], r['line']) for r in rows], [(5, 'five')])
        with open(calls) as f: args = [json.loads(line) for line in f]
        self.assertIn('--until', args[0]);self.assertNotIn('--follow', args[0])
        self.assertIn('--follow', args[1])
        self.assertEqual(args[0][args[0].index('--since') + 1], stamp)
    def test_lost_docker_restart_checkpoint_reports_history_loss(self):
        s = self.spool();s.append('2026-01-01T00:00:00Z', 'one')
        restored = self.spool()
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f: f.write('#!/bin/sh\nif [ "$1" = version ]; then echo 24.0.9; exit 0; fi\nexit 0\n')
        os.chmod(docker, 0o755)
        self.m.capture(restored, 'container', docker=docker)
        self.assertIn('history lost', restored.snapshot()[3])
        self.assertFalse(restored.snapshot()[2])
        self.assertIsNone(restored.snapshot(with_boundary=True)[4])
    def test_initial_finite_replay_timeout_reports_failure(self):
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f:
            f.write('#!/usr/bin/env python3\nimport sys\nif sys.argv[1:2] == ["version"]: print("24.0.9");sys.exit(0)\nimport time\ntime.sleep(10)\n')
        os.chmod(docker, 0o755)
        s = self.spool();self.m.capture(s, 'container', docker=docker, replay_timeout=0.2)
        self.assertIn('timed out', s.snapshot()[3])
        self.assertFalse(s.snapshot()[2])
        self.assertIsNone(s.snapshot(with_boundary=True)[4])
    def test_completed_helper_restart_keeps_boundary_without_docker(self):
        s = self.spool();s.append('2026-01-01T00:00:00Z', 'done');s.finish()
        restored = self.spool()
        self.m.capture(restored, 'container', docker='/nonexistent/docker')
        self.assertTrue(restored.snapshot()[2])
        self.assertEqual(restored.snapshot(with_boundary=True)[4], 1)
    def test_restart_deduplicates_nonmonotonic_docker_timestamps(self):
        one, two, three = ['2026-01-01T00:00:0%dZ' % i for i in [1, 2, 3]]
        s = self.spool()
        for ts, line in [(one, 'a'), (two, 'b'), (one, 'c')]: s.append(ts, line)
        restored = self.spool()
        self.assertEqual(restored.checkpoint(), (two, 2),
                         'checkpoint must count all records after the highest timestamp')
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f:
            f.write('#!/usr/bin/env python3\nimport sys\nif sys.argv[1:2] == ["version"]: print("24.0.9");sys.exit(0)\nimport sys\n'
                    'rows=' + repr([(two, 'b'), (one, 'c'), (three, 'd')]) + '\n'
                    'if "--follow" in sys.argv: rows=' + repr([(three, 'd'), (two, 'e')]) + '\n'
                    'for ts,line in rows: print(ts+" "+line)\n')
        os.chmod(docker, 0o755)
        self.m.capture(restored, 'container', docker=docker)
        rows, _, complete, error = restored.snapshot()
        self.assertEqual(error, '')
        self.assertTrue(complete)
        self.assertEqual([r['line'] for r in rows], ['a', 'b', 'c', 'd', 'e'])
    def test_changed_equal_timestamp_restart_overlap_reports_loss(self):
        stamp = '2026-01-01T00:00:00Z'
        s = self.spool();s.append(stamp, 'old-one');s.append(stamp, 'old-two')
        restored = self.spool()
        docker = self.tmp.name + '/docker'
        with open(docker, 'w') as f:
            f.write('#!/bin/sh\nif [ "$1" = version ]; then echo 24.0.9; exit 0; fi\nprintf "' + stamp + ' replacement-one\\n' + stamp + ' replacement-two\\n"\n')
        os.chmod(docker, 0o755)
        self.m.capture(restored, 'container', docker=docker)
        self.assertIn('history lost', restored.snapshot()[3])
        self.assertFalse(restored.snapshot()[2])
