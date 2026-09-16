#!/usr/bin/env python3
"""Build an immutable, tested package from the checked-out fork release branch."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import tarfile


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def main():
    repo = Path(__file__).resolve().parents[2]
    def git(*args):
        return subprocess.check_output(['git', '-C', str(repo)] + list(args), text=True).strip()
    if git('branch', '--show-current') != 'release' or git('status', '--porcelain'):
        raise RuntimeError('build requires a clean checked-out release branch')
    commit = git('rev-parse', 'HEAD')
    if commit != git('rev-parse', 'origin/release'):
        raise RuntimeError('release must match origin/release')
    root = Path(sys.argv[1]).resolve()
    root.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix='fork-'+commit[:12]+'-', dir=root))
    source = work / 'source'
    source.mkdir()
    archive = work / 'source.tar.gz'
    subprocess.run(['git', '-C', str(repo), 'archive', '--format=tar.gz', '--output='+str(archive), commit], check=True)
    subprocess.run(['tar', '-xzf', str(archive), '-C', str(source)], check=True)
    env = os.environ.copy()
    for key in ('GOROOT', 'GOOS', 'GOARCH'):
        env.pop(key, None)
    go, pnpm = env['GO_BIN'], env['PNPM_BIN']
    version = git('show', f'{commit}:backend/cmd/server/VERSION').strip() + '-fork.' + commit[:12]
    with (work/'build.log').open('w') as log:
        def run(args, cwd=source, extra=None):
            log.write('COMMAND '+repr(args)+'\n'); log.flush()
            subprocess.run(args, cwd=cwd, env=extra or env, stdout=log, stderr=subprocess.STDOUT, check=True)
        run([go, 'version']); run(['node', '--version']); run([pnpm, '--version'])
        run([pnpm, 'install', '--frozen-lockfile'], source/'frontend')
        run([pnpm, 'build'], source/'frontend')
        selector = 'TestOpenAIAttestation|TestOpenAIWSPool_Attestation|TestNormalizeHeaderOverrideCredentials|TestMappedGPT55|TestNormalizeOpenAIResponsesLite|TestApplyCodexOAuthTransform_PreservesLiteNamespaceToolChoice|TestOpenAIGatewayServiceForward_.*ResponsesLite|TestOpenAIBuildUpstreamRequestOpenAIPassthroughForwardsResponsesLiteHeader|TestProxyOpenAIWSHTTPBridgeTurn'
        with (work/'tests.jsonl').open('w') as events:
            subprocess.run([go,'test','-tags=unit','./internal/service','-run',selector,'-count=1','-json'], cwd=source/'backend', env=env, stdout=events, stderr=log, check=True)
        passed = [json.loads(line) for line in (work/'tests.jsonl').read_text().splitlines() if line.strip()]
        names = {e.get('Test') for e in passed if e.get('Action') == 'pass'}
        required = ['TestOpenAIAttestationHTTPForwardingModeAndTarget', 'TestOpenAIAttestationHTTPForwardingRemovesNonCanonicalExistingHeader', 'TestOpenAIWSPool_AttestationScopeIsolatedAndRawValueIsNotPrewarmed', 'TestNormalizeHeaderOverrideCredentials']
        if not all(n in names for n in required) or not any(n and n.startswith('TestMappedGPT55') for n in names):
            raise RuntimeError('required tests did not execute')
        cross = dict(env, CGO_ENABLED='0', GOOS='linux', GOARCH='amd64')
        run([go,'build','-tags','embed','-trimpath','-ldflags','-s -w -X main.Version='+version+' -X main.Commit='+commit+' -X main.BuildType=release','-o',str(work/'sub2api'),'./cmd/server'], source/'backend', cross)
    runner = (source/'backend/internal/repository/migrations_runner.go').read_text()
    rules = {name: re.findall(r'"([0-9a-f]{64})"', values) for name, values in re.findall(r'"([^"\n]+\.sql)":\s*newMigrationChecksumCompatibilityRule\(([^\n]+)\)', runner)}
    migrations = {}
    for path in sorted((source/'backend/migrations').glob('*.sql')):
        content = path.read_text().strip()
        if content:
            sha = hashlib.sha256(content.encode()).hexdigest()
            migrations[path.name] = {'checksum':sha, 'accepted': rules.get(path.name, []) if sha in rules.get(path.name, []) else []}
    manifest = {'format':'fork-release-v1','status':'BUILT_TESTED','source_commit':commit,'source_tree':git('rev-parse','HEAD^{tree}'),'build_version':version,'mode':'http','required_tests':required,'tests_passed':len(names),'migrations':migrations,'files':{name:digest(work/name) for name in ('sub2api','source.tar.gz','build.log','tests.jsonl')}}
    (work/'manifest.json').write_text(json.dumps(manifest, indent=2)+'\n')
    files = list(manifest['files']) + ['manifest.json']
    (work/'SHA256SUMS').write_text(''.join(digest(work/name)+'  '+name+'\n' for name in files))
    with tarfile.open(work/'release-linux-amd64.tar.gz','w:gz') as package:
        for name in files + ['SHA256SUMS']:
            package.add(work/name, arcname=name)
    print('BUILT='+str(work), flush=True)


if __name__ == '__main__':
    main()
