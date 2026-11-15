"""Verify and build the published New API source snapshot (AGPL-3.0).

Copyright (c) 2026 AI Relay project contributors.
See LICENSE and NOTICE in the source release.
"""
import argparse
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import subprocess
import tarfile
import tempfile


def source_bytes(data, expected):
    # Git attributes and Windows checkouts can normalize line endings. Accept
    # only a variant that restores the exact original bytes recorded at build.
    normalized = data.replace(b'\r\n', b'\n')
    for candidate in (data, normalized, normalized.replace(b'\n', b'\r\n')):
        if hashlib.sha256(candidate).hexdigest() == expected:
            return candidate
    raise ValueError('Source content changed')


def context(root, destination):
    root = Path(root).resolve()
    manifest = json.loads((root / 'SOURCE-MANIFEST.json').read_text(encoding='utf-8'))
    source = root / 'new-api'
    with tarfile.open(destination, 'w') as output:
        for name, metadata in manifest['files'].items():
            relative = PurePosixPath(name)
            if relative.is_absolute() or '..' in relative.parts or '\\' in name or ':' in name:
                raise ValueError('Unsafe source path')
            path = source.joinpath(*relative.parts)
            if path.is_symlink() or not path.resolve().is_relative_to(source.resolve()):
                raise ValueError('Source escaped snapshot: ' + name)
            try:
                data = source_bytes(path.read_bytes(), metadata['sha256'])
            except ValueError as error:
                raise ValueError('Source changed: ' + name) from error
            item = tarfile.TarInfo(name)
            item.size = len(data)
            item.mode = metadata['mode']
            output.addfile(item, io.BytesIO(data))
    with open(destination, 'rb') as bundle:
        actual = hashlib.file_digest(bundle, 'sha256').hexdigest()
    if actual != manifest['contextSHA256']:
        raise ValueError('Build context does not match the published version')
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--verify-only', action='store_true')
    parser.add_argument('--tag', default='ai-relay/new-api:public-source')
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    with tempfile.TemporaryDirectory(prefix='relay-source-') as temporary:
        archive = Path(temporary) / 'context.tar'
        manifest = context(root, archive)
        print('Verified complete source and build context: ' + manifest['contextSHA256'], flush=True)
        if not args.verify_only:
            with archive.open('rb') as bundle:
                subprocess.run(['docker', 'build', '--label',
                                'org.opencontainers.image.revision=' + manifest['upstreamRevision'],
                                '-t', args.tag, '-'], stdin=bundle, check=True)


if __name__ == '__main__':
    main()
