#!/usr/bin/env python3
"""Interleaved, SHA-256-checked two-host Flock/croc benchmarks (Python stdlib)."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import socket
import subprocess
import tempfile
import threading
import time
import uuid

HASHER = '''import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1]);files={}
for p in sorted(root.rglob('*')):
 if p.is_file():
  h=hashlib.sha256()
  with p.open('rb') as f:
   for block in iter(lambda:f.read(1<<20),b''):h.update(block)
  files[p.relative_to(root).as_posix()]=[p.stat().st_size,h.hexdigest()]
print(json.dumps({'files':files,'dirs':sorted(p.relative_to(root).as_posix() for p in root.rglob('*') if p.is_dir())}))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--ssh', required=True, help='existing SSH alias with machine-key access')
    parser.add_argument('--local-ip', required=True, help='IPv4 reachable from the other host')
    parser.add_argument('--remote-ip', required=True)
    parser.add_argument('--flock', required=True, help='local candidate binary')
    parser.add_argument('--remote-flock', required=True, help='already staged remote binary')
    parser.add_argument('--croc', required=True, help='local pinned croc binary (tested: 11.5.4)')
    parser.add_argument('--remote-croc', required=True)
    parser.add_argument('--baseline-flock', help='optional installed baseline binary')
    parser.add_argument('--remote-baseline-flock')
    parser.add_argument('--variants', default='flock,croc4,croc8')
    parser.add_argument('--fixtures', default='startup,large,small')
    parser.add_argument('--directions', default='forward,reverse')
    parser.add_argument('--rounds', type=int, default=3)
    parser.add_argument('--mib', type=int, default=1024)
    parser.add_argument('--files', type=int, default=1024)
    parser.add_argument('--streams', type=int, default=8)
    parser.add_argument('--debug', action='store_true', help='private debug logs; do not use for timing claims')
    parser.add_argument('--output', type=Path, required=True, help='public metrics only, JSON Lines')
    args = parser.parse_args()
    if args.rounds < 1 or args.mib < 1 or args.files < 1 or not 1 <= args.streams <= 16:
        parser.error('positive rounds/mib/files and 1..16 streams are required')
    variants, fixtures, directions = (value.split(',') for value in
                                     (args.variants, args.fixtures, args.directions))
    if set(variants) - {'flock', 'baseline', 'croc4', 'croc8', 'croczip'}:
        parser.error('variants: flock,baseline,croc4,croc8,croczip')
    if set(fixtures) - {'startup', 'large', 'small'} or set(directions) - {'forward', 'reverse'}:
        parser.error('fixtures: startup,large,small; directions: forward,reverse')
    if 'baseline' in variants and not (args.baseline_flock and args.remote_baseline_flock):
        parser.error('baseline requires both baseline binary paths')
    ssh = ['ssh', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', args.ssh]

    def command(remote, argv):
        if args.debug:
            argv = ['env', 'LOGGER=debug', *argv]
        return ssh + [shlex.join(argv)] if remote else argv

    def run(remote, argv, **kwargs):
        return subprocess.run(command(remote, argv), check=True, **kwargs)

    # Logs may contain bearer invitations. Retain failures privately; publish only metrics.
    work = Path(tempfile.mkdtemp(prefix='flock-benchmark-'))
    print(f'Private logs and fixtures: {work}', flush=True)
    source = work / 'source'
    source.mkdir()
    if 'startup' in fixtures:
        (source / 'startup.bin').write_bytes(os.urandom(65536))
    if 'large' in fixtures:
        with (source / 'large.bin').open('wb') as file:
            for _ in range(args.mib):
                file.write(os.urandom(1 << 20))
    if 'small' in fixtures:
        small = source / 'small'
        small.mkdir()
        for index in range(args.files):
            (small / f'{index:04}.bin').write_bytes(os.urandom(4096 + index % 97))
        (small / 'empty').mkdir()
        (small / 'zero').touch()
        (small / 'café 日本語.txt').write_text('Unicode\n')
    remote_root = '/tmp/flock-benchmark-' + uuid.uuid4().hex
    run(True, ['mkdir', '-m', '700', remote_root])
    (work / 'run.json').write_text(json.dumps(dict(vars(args), remote_root=remote_root), default=str, indent=2))
    # Stage identical immutable source bytes outside all measured intervals.
    subprocess.run(['scp', '-qr', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes',
                    str(source), args.ssh + ':' + remote_root + '/source'], check=True)
    expected = {}
    names = {'startup': 'startup.bin', 'large': 'large.bin', 'small': 'small'}
    inventory = json.loads(run(False, ['python3', '-c', HASHER, str(source)], capture_output=True,
                               text=True).stdout)
    for fixture in fixtures:
        name = names[fixture]
        expected[fixture] = {path: value for path, value in inventory['files'].items()
                             if path == name or path.startswith(name + '/')}
    # One persistent controller-hosted relay for every tool. Croc never uses discovery
    # or public services, and its data transport is explicitly TCP relay.
    for port in range(22100, 23100, 10):
        sockets = []
        try:
            for offset in range(9):
                sock = socket.socket()
                sockets.append(sock)
                sock.bind(('0.0.0.0', port + offset))
            break
        except OSError:
            pass
        finally:
            for sock in sockets:
                sock.close()
    else:
        raise RuntimeError('no free relay port block')
    with (work / 'relay.log').open('wb') as log:
        relay = subprocess.Popen([args.croc, 'relay', '--host', '0.0.0.0', '--ports',
                                  ','.join(str(port + i) for i in range(9))], stdout=log, stderr=log)
        try:
            for _ in range(100):
                try:
                    with socket.create_connection(('127.0.0.1', port), timeout=.1):
                        break
                except OSError:
                    time.sleep(.03)
            else:
                raise RuntimeError('relay did not start')
            for iteration in range(args.rounds):
                order = variants if iteration % 2 == 0 else list(reversed(variants))
                for direction in directions:
                    for fixture in fixtures:
                        for variant in order:
                            remote_send = direction == 'reverse'
                            remote_receive = not remote_send
                            root = remote_root if remote_send else str(work)
                            receive_root = str(work) if remote_send else remote_root
                            item = root + '/source/' + names[fixture]
                            run_id = uuid.uuid4().hex
                            dest = receive_root + '/' + run_id
                            is_croc = variant.startswith('croc')
                            common = ['--relay', args.local_ip + ':' + str(port), '--yes',
                                      '--no-compress', '--ignore-stdin', '--disable-clipboard']
                            if is_croc:
                                run(remote_receive, ['mkdir', dest])
                                local, remote = args.croc, args.remote_croc
                                secret = 'bench-' + uuid.uuid4().hex
                                environment = ['env', 'CROC_SECRET=' + secret,
                                               'CROC_CONFIG_DIR=' + root + '/config']
                                send = environment + [remote if remote_send else local, *common,
                                    'send', '--no-local', '--transport', 'relay', '--transfers',
                                    '4' if variant == 'croc4' else '8',
                                    *(['--zip'] if variant == 'croczip' else []), item]
                                receive = ['env', 'CROC_SECRET=' + secret,
                                    'CROC_CONFIG_DIR=' + receive_root + '/config',
                                    remote if remote_receive else local, *common, '--out', dest]
                            else:
                                local, remote = ((args.baseline_flock, args.remote_baseline_flock)
                                                 if variant == 'baseline' else (args.flock, args.remote_flock))
                                send = [remote if remote_send else local, 'send', '--relay',
                                    args.local_ip + ':' + str(port), '--advertise',
                                    args.remote_ip if remote_send else args.local_ip, '--streams',
                                    str(args.streams), '--timeout', '120s', item]
                                receive = [remote if remote_receive else local, 'receive',
                                           '--out', dest, '--timeout', '120s']
                            # Bound remote tools too; terminating SSH alone can leave a remote child.
                            timeout_wrapper = ['python3', '-c',
                                'import subprocess,sys; sys.exit(subprocess.run(sys.argv[1:],timeout=125).returncode)']
                            with (work / (run_id + '-send.log')).open('w') as send_log, \
                                 (work / (run_id + '-receive.log')).open('w') as recv_log:
                                started = time.perf_counter()
                                sender = subprocess.Popen(command(remote_send, timeout_wrapper + send),
                                    stdin=subprocess.DEVNULL, stdout=send_log if is_croc else subprocess.PIPE,
                                    stderr=send_log, text=True)
                                try:
                                    invitation, prepared, remainder = None, None, []
                                    if not is_croc:
                                        for line in sender.stdout:
                                            event = json.loads(line)
                                            if event['event'] == 'error':
                                                raise RuntimeError(event['message'])
                                            if event['event'] == 'invitation':
                                                invitation = event
                                                prepared = time.perf_counter() - started
                                                break
                                        else:
                                            raise RuntimeError('sender exited before invitation')
                                        # Keep draining stdout while the receiver runs: pipe capacity
                                        # must never become a hidden transfer limit.
                                        drain = threading.Thread(target=lambda stream=sender.stdout, lines=remainder: lines.extend(stream), daemon=True)
                                        drain.start()
                                    received = subprocess.run(command(remote_receive, timeout_wrapper + receive),
                                        input=json.dumps(invitation) if invitation else None,
                                        stdout=subprocess.PIPE, stderr=recv_log, text=True, timeout=130)
                                    sender.wait(timeout=15)
                                    if not is_croc:
                                        drain.join(timeout=5)
                                    elapsed = time.perf_counter() - started
                                    if sender.returncode or received.returncode:
                                        recv_log.write(received.stdout)
                                        send_log.write(''.join(remainder))
                                        raise RuntimeError(f'{variant} failed (sender={sender.returncode}, receiver={received.returncode}); see private logs {run_id}')
                                    if not is_croc:
                                        events = [json.loads(line) for line in remainder]
                                        recipient = [json.loads(line) for line in received.stdout.splitlines()]
                                        assert any(e.get('status') == 'quic_connected' for e in events)
                                        assert any(e['event'] == 'completed' and e.get('sha256_verified') for e in recipient)
                                    actual = json.loads(run(remote_receive, ['python3', '-c', HASHER, dest],
                                                            capture_output=True, text=True).stdout)
                                    assert actual['files'] == expected[fixture], 'independent SHA-256 mismatch'
                                    if fixture == 'small':
                                        assert 'small/empty' in actual['dirs']
                                    row = dict(variant=variant, direction=direction, fixture=fixture, round=iteration,
                                        bytes=sum(value[0] for value in expected[fixture].values()),
                                        files=len(expected[fixture]), command_seconds=round(elapsed, 6),
                                        verified_seconds=round(time.perf_counter() - started, 6),
                                        prepare_seconds=round(prepared, 6) if prepared else None,
                                        independent_sha256=True, transport='tcp' if is_croc else 'quic/udp')
                                    with args.output.open('a') as output:
                                        output.write(json.dumps(row) + '\n')
                                    print(json.dumps(row), flush=True)
                                finally:
                                    if sender.poll() is None:
                                        sender.kill()
                                        sender.wait()
        finally:
            relay.terminate()
            relay.wait(timeout=10)
    print('Remote private fixtures (remove when finished): ' + remote_root)


if __name__ == '__main__':
    main()
