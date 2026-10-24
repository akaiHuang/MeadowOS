"""Bounded, non-executing local file previews for the Meadow desktop.

This module has no GUI dependency and runs in the caller's isolated worker.
Office previews expose actual text/tables, not a claim of full layout fidelity.
Archive members are never extracted and HTML/scripts are shown only as text.
"""
from __future__ import annotations

import bz2
import csv
import ctypes
from contextlib import contextmanager
from datetime import datetime, timezone
from functools import lru_cache
import gzip
import hashlib
import io
import json
import lzma
import mimetypes
import os
from pathlib import Path, PurePosixPath
import posixpath
import re
import shutil
import signal
import stat
import struct
import sys
import subprocess
import tarfile
import tempfile
import xml.etree.ElementTree as ET
import zipfile

from PIL import Image, ImageDraw, ImageFont, ImageOps

MAX_READ = 256 * 1024
MAX_TEXT = 64 * 1024
MAX_IMAGE_BYTES = 32 * 1024 * 1024
MAX_PIXELS = 16_000_000
MAX_EDGE = 1600
MAX_ZIP_BYTES = 64 * 1024 * 1024
MAX_ENTRIES = 2048
MAX_PART = 4 * 1024 * 1024
MAX_EXPANDED = 16 * 1024 * 1024
MAX_ROWS, MAX_COLUMNS, MAX_CELL = 100, 12, 512
MAX_PAGES = 256
PROCESS_TIMEOUT = 7
_LIBC = ctypes.CDLL(None, use_errno=True) if sys.platform.startswith('linux') else None
OFFICE = {'.docx': 'document', '.odt': 'document', '.xlsx': 'spreadsheet',
          '.ods': 'spreadsheet', '.pptx': 'presentation', '.odp': 'presentation'}
CODE = {'.py', '.js', '.ts', '.tsx', '.jsx', '.go', '.rs', '.c', '.h', '.cpp',
        '.css', '.html', '.htm', '.xml', '.json', '.yaml', '.yml', '.toml', '.sh',
        '.bash', '.zsh', '.sql', '.swift', '.java', '.php', '.rb', '.ipynb'}
TEXT = {'.txt', '.md', '.markdown', '.log', '.rst', '.ini', '.conf', '.env'}
MEDIA_FORMATS = 'mov,matroska,webm,avi,mp3,wav,flac,ogg,aac,aiff,mpeg,mpegts,flv'


class PreviewError(ValueError):
    """An unsupported or unsafe preview boundary, safe to show as metadata."""


def category_for(path, mime=None):
    """Return a stable visual category without reading or running the file."""
    value = os.fspath(path)
    suffix = Path(value).suffix.lower()
    mime = mime or mimetypes.guess_type(value)[0] or ''
    if suffix in OFFICE:
        return OFFICE[suffix]
    if suffix == '.pdf' or mime == 'application/pdf':
        return 'pdf'
    if suffix in {'.csv', '.tsv', '.xls'}:
        return 'spreadsheet'
    if suffix in {'.doc', '.rtf'}:
        return 'document'
    if suffix in {'.ppt', '.pps'}:
        return 'presentation'
    if suffix in CODE:
        return 'code'
    if suffix in TEXT or mime.startswith('text/'):
        return 'text'
    if suffix in {'.zip', '.tar', '.gz', '.tgz', '.bz2', '.xz', '.7z', '.rar'}:
        return 'archive'
    if suffix in {'.ttf', '.otf', '.woff', '.woff2', '.ttc'}:
        return 'font'
    if suffix in {'.exe', '.dll', '.so', '.dylib', '.appimage', '.deb', '.rpm'}:
        return 'executable'
    for category in ('image', 'audio', 'video'):
        if mime.startswith(category + '/'):
            return category
    if mime == 'inode/directory':
        return 'folder'
    return 'unknown'


def _local_path(path):
    raw = os.fspath(path)
    if not isinstance(raw, str) or '\x00' in raw or '://' in raw or raw.startswith('//'):
        raise PreviewError('Only local filesystem paths can be previewed.')
    return Path(os.path.abspath(os.path.expanduser(raw)))


@contextmanager
def _regular(path):
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0) | getattr(os, 'O_NONBLOCK', 0)
    if path.is_symlink():
        raise PreviewError('Symbolic links are not followed by the preview worker.')
    fd = os.open(path, flags)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode):
            raise PreviewError('This file type cannot be read as a preview.')
        yield stream, info
        after = os.fstat(stream.fileno())
        if (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) != (
                after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
            raise PreviewError('The file changed while its preview was being read.')


def _base(path, info, mime):
    return {'kind': 'unsupported', 'title': path.name or str(path), 'mime': mime,
            'metadata': {'filename': path.name, 'modified': datetime.fromtimestamp(
                info.st_mtime, timezone.utc).isoformat()}, 'size': info.st_size,
            'page': 0, 'page_count': 1}


def _page(value, count):
    try:
        return min(max(int(value), 0), max(count - 1, 0))
    except (TypeError, ValueError, OverflowError):
        return 0


def _cache_root():
    root = Path(os.environ.get('XDG_CACHE_HOME', str(Path.home() / '.cache'))) / 'meadow' / 'file-preview'
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if root.is_symlink() or root.stat().st_uid != os.getuid():
        raise PreviewError('The preview cache must be owned by the current user.')
    return root


def _atomic_png(image, output):
    output = Path(output)
    parent = output.parent
    if not parent.is_dir() or parent.is_symlink() or parent.stat().st_uid != os.getuid():
        raise PreviewError('Thumbnail output needs a user-owned cache directory.')
    if output.is_symlink() or (output.exists() and not output.is_file()):
        raise PreviewError('Thumbnail output must be a regular cache file.')
    fd, temporary = tempfile.mkstemp(prefix='.meadow-preview-', suffix='.png', dir=parent)
    try:
        with os.fdopen(fd, 'wb') as target:
            image.save(target, format='PNG')
        os.replace(temporary, output)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def _save_image(image, path, info, page):
    digest = hashlib.sha256(f'{path}:{info.st_dev}:{info.st_ino}:{info.st_size}:{info.st_mtime_ns}:{page}:v1'.encode()).hexdigest()
    output = _cache_root() / (digest + '.png')
    image.thumbnail((MAX_EDGE, MAX_EDGE), Image.Resampling.LANCZOS)
    _atomic_png(image.convert('RGBA'), output)
    return str(output)


def _bounded_image(stream, path, info, result):
    if info.st_size > MAX_IMAGE_BYTES:
        raise PreviewError('Image exceeds the preview input size limit.')
    with Image.open(stream) as image:
        width, height = image.size
        if width <= 0 or height <= 0 or width * height > MAX_PIXELS:
            raise PreviewError('Image exceeds the preview pixel limit.')
        result['metadata'].update(width=width, height=height, format=image.format)
        image = ImageOps.exif_transpose(image)
        result.update(kind='image', image_path=_save_image(image, path, info, 0))
    return result


def _decoder_child(expected_parent):
    # This backend is run in a dedicated worker process. On Linux an outer
    # cancellation must kill its decoder even before our own timeout fires.
    if _LIBC is not None:
        if _LIBC.prctl(1, signal.SIGKILL, 0, 0, 0) != 0 or os.getppid() != expected_parent:
            os._exit(126)


def _run(arguments, stream):
    """No shell, no stdin, bounded time and bounded captured output."""
    executable = shutil.which(arguments[0])
    if not executable:
        raise PreviewError(f'{arguments[0]} is not installed for this preview.')
    parent_pid = os.getpid()
    with tempfile.TemporaryFile() as output:
        process = subprocess.Popen([executable, *arguments[1:]], stdin=subprocess.DEVNULL,
                                   stdout=output, stderr=subprocess.DEVNULL,
                                   pass_fds=(stream.fileno(),), start_new_session=True,
                                   preexec_fn=(lambda: _decoder_child(parent_pid)) if _LIBC is not None else None)
        try:
            process.wait(timeout=PROCESS_TIMEOUT)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            raise PreviewError('The preview decoder reached its time limit.') from None
        if process.returncode:
            raise PreviewError('This file could not be decoded for a preview.')
        output.seek(0)
        data = output.read(MAX_READ + 1)
        if len(data) > MAX_READ:
            raise PreviewError('Decoder metadata exceeds the preview size limit.')
        return data


def _fd_path(stream):
    return f'/proc/self/fd/{stream.fileno()}' if Path('/proc/self/fd').exists() else f'/dev/fd/{stream.fileno()}'


def _pdf(stream, path, info, result, page):
    if info.st_size > 256 * 1024 * 1024:
        raise PreviewError('PDF exceeds the preview input size limit.')
    description = _run(['pdfinfo', _fd_path(stream)], stream).decode('utf-8', 'replace')
    match = re.search(r'^Pages:\s*(\d+)', description, re.MULTILINE)
    if not match:
        raise PreviewError('PDF page count is unavailable.')
    count = max(1, min(int(match.group(1)), 10000))
    page = _page(page, count)
    with tempfile.TemporaryDirectory(prefix='pdf-', dir=_cache_root()) as temporary:
        output = str(Path(temporary) / 'page')
        _run(['pdftoppm', '-f', str(page + 1), '-l', str(page + 1), '-singlefile',
              '-scale-to', str(MAX_EDGE), '-png', '-cropbox', _fd_path(stream), output], stream)
        with Image.open(output + '.png') as image:
            if image.width * image.height > MAX_PIXELS:
                raise PreviewError('PDF decoder output exceeds the pixel limit.')
            result.update(kind='image', page=page, page_count=count,
                          image_path=_save_image(image, path, info, page))
    result['metadata']['preview_fidelity'] = 'rendered PDF page'
    return result


def _decode(data):
    if data.startswith((b'\xff\xfe', b'\xfe\xff')):
        return data.decode('utf-16', 'replace')
    if b'\x00' in data:
        raise PreviewError('Binary data is not displayed as document text.')
    text = data.decode('utf-8-sig', 'replace')
    controls = sum(ord(char) < 32 and char not in '\r\n\t\f' for char in text)
    if controls > max(2, len(text) // 100):
        raise PreviewError('Binary data is not displayed as document text.')
    return text


def _text(stream, result, suffix):
    data = stream.read(MAX_READ + 1)
    value = _decode(data[:MAX_READ])
    result['metadata'].update(truncated=len(data) > MAX_READ or len(value) > MAX_TEXT,
                              preview_fidelity='plain text; active content is not executed')
    if suffix in {'.csv', '.tsv'}:
        try:
            dialect = csv.excel_tab if suffix == '.tsv' else csv.Sniffer().sniff(value[:4096])
        except csv.Error:
            dialect = csv.excel
        try:
            reader = csv.reader(io.StringIO(value), dialect)
            columns = next(reader, [])[:MAX_COLUMNS]
            rows = []
            for row in reader:
                if len(rows) >= MAX_ROWS:
                    result['metadata']['truncated'] = True
                    break
                rows.append([cell[:MAX_CELL] for cell in row[:MAX_COLUMNS]])
            result.update(kind='table', columns=[c[:MAX_CELL] for c in columns], rows=rows)
            return result
        except csv.Error:
            pass
    result.update(kind='text', text=value[:MAX_TEXT])
    return result


def _safe_member(name):
    path = PurePosixPath(name)
    return bool(name) and bool(path.parts) and '\\' not in name and '\x00' not in name and not path.is_absolute() and '..' not in path.parts and ':' not in path.parts[0]


class _Zip:
    def __init__(self, stream, size):
        if size > MAX_ZIP_BYTES:
            raise PreviewError('Archive exceeds the preview input size limit.')
        stream.seek(max(0, size - 65557))
        tail = stream.read(65557)
        end = tail.rfind(b'PK\x05\x06')
        if end < 0 or len(tail) - end < 22:
            raise PreviewError('Archive has no supported central directory.')
        _, disk, cd_disk, disk_count, count, cd_size, _, comment = struct.unpack_from('<4s4H2LH', tail, end)
        if disk or cd_disk or disk_count != count or count > MAX_ENTRIES or cd_size > 8 * 1024 * 1024 or end + 22 + comment != len(tail):
            raise PreviewError('Archive directory exceeds the preview limits.')
        stream.seek(0)
        self.archive = zipfile.ZipFile(stream)
        if len(self.archive.infolist()) > MAX_ENTRIES:
            self.archive.close()
            raise PreviewError('Archive entry count exceeds the preview limit.')
        self.members, self.budget = {}, MAX_EXPANDED
        total = 0
        for member in self.archive.infolist():
            total += member.file_size
            mode = member.external_attr >> 16
            if (not _safe_member(member.filename) or member.filename in self.members or
                    stat.S_ISLNK(mode) or member.flag_bits & 1 or member.file_size > MAX_EXPANDED or
                    total > MAX_EXPANDED or member.file_size > max(1024 * 1024, member.compress_size * 200)):
                self.archive.close()
                raise PreviewError('Archive contains unsafe names or exceeds decompression limits.')
            self.members[member.filename] = member

    def read(self, name):
        member = self.members.get(name)
        if member is None:
            raise PreviewError('A required document part is missing.')
        if member.file_size > MAX_PART or member.file_size > self.budget:
            raise PreviewError('Document part exceeds the preview size limit.')
        with self.archive.open(member) as entry:
            data = entry.read(MAX_PART + 1)
        if len(data) > MAX_PART:
            raise PreviewError('Document part exceeds the preview size limit.')
        self.budget -= len(data)
        return data

    def xml(self, name):
        data = self.read(name)
        upper = data.upper()
        if b'\x00' in data or b'<!DOCTYPE' in upper or b'<!ENTITY' in upper:
            raise PreviewError('DTD and entity declarations are not supported in document previews.')
        root = ET.fromstring(data)
        if sum(1 for _ in root.iter()) > 40000:
            raise PreviewError('Document XML exceeds the preview node limit.')
        pending = [(root, 0)]
        while pending:
            node, depth = pending.pop()
            if depth > 64:
                raise PreviewError('Document XML exceeds the preview nesting limit.')
            pending.extend((child, depth + 1) for child in node)
        return root


def _local(tag):
    return tag.rsplit('}', 1)[-1]


def _node_text(node, limit=MAX_CELL):
    chunks, remaining = [], limit
    for text in node.itertext():
        chunks.append(text[:remaining])
        remaining -= len(chunks[-1])
        if remaining <= 0:
            break
    return ''.join(chunks)


def _paragraphs(root):
    lines, remaining = [], MAX_TEXT
    for node in root.iter():
        if _local(node.tag) in {'p', 'h'}:
            text = _node_text(node, remaining)
            lines.append(text)
            remaining -= len(text) + 1
            if remaining <= 0:
                break
    return '\n'.join(lines)[:MAX_TEXT]


def _relationships(archive, name, base):
    result = {}
    for node in archive.xml(name):
        target = node.get('Target', '')
        if node.get('TargetMode') == 'External' or ':' in target or '\\' in target:
            continue
        joined = posixpath.normpath(posixpath.join(base, target)) if not target.startswith('/') else target.lstrip('/')
        if _safe_member(joined):
            result[node.get('Id')] = joined
    return result


def _relation_id(node):
    return next((value for key, value in node.attrib.items() if key.endswith('}id')), None)


def _column_name(index):
    result = ''
    while index >= 0:
        result = chr(65 + index % 26) + result
        index = index // 26 - 1
    return result


def _xlsx(archive, result, page):
    workbook = archive.xml('xl/workbook.xml')
    relationships = _relationships(archive, 'xl/_rels/workbook.xml.rels', 'xl')
    sheets = [node for node in workbook.iter() if _local(node.tag) == 'sheet'][:MAX_PAGES]
    if not sheets:
        raise PreviewError('Workbook has no supported sheets.')
    page = _page(page, len(sheets))
    sheet = sheets[page]
    target = relationships.get(_relation_id(sheet))
    if not target:
        raise PreviewError('Workbook sheet relationship is unavailable.')
    shared = []
    if 'xl/sharedStrings.xml' in archive.members:
        shared = [_node_text(node) for node in archive.xml('xl/sharedStrings.xml')
                  if _local(node.tag) == 'si']
    rows, indices, row_numbers = [], set(), []
    for row in archive.xml(target).iter():
        if _local(row.tag) != 'row':
            continue
        if len(rows) >= MAX_ROWS:
            result['metadata']['truncated'] = True
            break
        cells = {}
        for cell in row:
            if _local(cell.tag) != 'c':
                continue
            coordinate = re.match(r'^([A-Z]{1,4})(\d+)$', cell.get('r', ''))
            if not coordinate:
                continue
            column = 0
            for char in coordinate[1]:
                column = column * 26 + ord(char) - 64
            column -= 1
            if column not in indices and len(indices) >= MAX_COLUMNS:
                result['metadata']['truncated'] = True
                continue
            indices.add(column)
            value = next((node.text or '' for node in cell if _local(node.tag) == 'v'), '')
            if cell.get('t') == 's':
                try:
                    value = shared[int(value)] if 0 <= int(value) < len(shared) else ''
                except ValueError:
                    value = ''
            elif cell.get('t') == 'inlineStr':
                value = ''.join(node.text or '' for node in cell.iter() if _local(node.tag) == 't')
            elif not value:
                value = next(('=' + (node.text or '') for node in cell if _local(node.tag) == 'f'), '')
            cells[column] = value[:MAX_CELL]
        rows.append(cells)
        row_numbers.append(row.get('r', '')[:16])
    columns = sorted(indices)
    result.update(kind='table', page=page, page_count=len(sheets), columns=[_column_name(c) for c in columns],
                  rows=[[row.get(c, '') for c in columns] for row in rows])
    result['metadata'].update(sheet=sheet.get('name', '')[:MAX_CELL], row_numbers=row_numbers, formulas_evaluated=False)
    return result


def _ods(root, result, page):
    tables = [node for node in root.iter() if _local(node.tag) == 'table'][:MAX_PAGES]
    if not tables:
        raise PreviewError('Spreadsheet has no supported sheets.')
    page = _page(page, len(tables))
    table, rows = tables[page], []
    for row in table.iter():
        if _local(row.tag) != 'table-row':
            continue
        cells = []
        for cell in row:
            if _local(cell.tag) not in {'table-cell', 'covered-table-cell'}:
                continue
            attributes = {_local(k): v for k, v in cell.attrib.items()}
            value = _paragraphs(cell) or attributes.get('string-value', attributes.get('value', attributes.get('date-value', attributes.get('boolean-value', ''))))
            repeat = min(MAX_COLUMNS - len(cells), max(1, int(attributes.get('number-columns-repeated', '1'))))
            cells.extend([value[:MAX_CELL]] * repeat)
            if len(cells) >= MAX_COLUMNS:
                break
        attributes = {_local(k): v for k, v in row.attrib.items()}
        repeat = min(MAX_ROWS - len(rows), max(1, int(attributes.get('number-rows-repeated', '1'))))
        rows.extend([list(cells) for _ in range(repeat)])
        if len(rows) >= MAX_ROWS:
            result['metadata']['truncated'] = True
            break
    count = max((len(row) for row in rows), default=0)
    result.update(kind='table', page=page, page_count=len(tables), columns=[_column_name(i) for i in range(count)], rows=rows)
    result['metadata']['sheet'] = next((v for k, v in table.attrib.items() if _local(k) == 'name'), '')[:MAX_CELL]
    return result


def _office(stream, info, result, suffix, page):
    archive = _Zip(stream, info.st_size)
    try:
        result['metadata']['preview_fidelity'] = 'text/table preview; original document layout is not rendered'
        if suffix == '.docx':
            result.update(kind='text', text=_paragraphs(archive.xml('word/document.xml')))
        elif suffix == '.xlsx':
            return _xlsx(archive, result, page)
        elif suffix == '.pptx':
            presentation = archive.xml('ppt/presentation.xml')
            relationships = _relationships(archive, 'ppt/_rels/presentation.xml.rels', 'ppt')
            slides = [relationships.get(_relation_id(node)) for node in presentation.iter() if _local(node.tag) == 'sldId'][:MAX_PAGES]
            if not slides or any(not slide for slide in slides):
                raise PreviewError('Presentation slide order is unavailable.')
            page = _page(page, len(slides))
            result.update(kind='text', page=page, page_count=len(slides), text=_paragraphs(archive.xml(slides[page])))
        else:
            root = archive.xml('content.xml')
            if suffix == '.ods':
                return _ods(root, result, page)
            if suffix == '.odp':
                pages = [node for node in root.iter() if _local(node.tag) == 'page'][:MAX_PAGES]
                if not pages:
                    raise PreviewError('Presentation has no supported slides.')
                page = _page(page, len(pages))
                result.update(kind='text', page=page, page_count=len(pages), text=_paragraphs(pages[page]))
            else:
                result.update(kind='text', text=_paragraphs(root))
        return result
    finally:
        archive.archive.close()


class _LimitedReader:
    def __init__(self, stream):
        self.stream, self.total = stream, 0

    def read(self, size=-1):
        size = min(MAX_EXPANDED + 1 - self.total, size if size >= 0 else MAX_EXPANDED + 1)
        data = self.stream.read(size)
        self.total += len(data)
        if self.total > MAX_EXPANDED:
            raise PreviewError('Archive expansion exceeds the preview limit.')
        return data


def _archive(stream, info, result, suffix):
    if info.st_size > MAX_ZIP_BYTES:
        raise PreviewError('Archive exceeds the preview input size limit.')
    rows = []
    if suffix == '.zip':
        archive = _Zip(stream, info.st_size)
        try:
            for member in list(archive.members.values())[:MAX_ROWS]:
                rows.append([member.filename[:MAX_CELL], str(member.file_size), 'folder' if member.is_dir() else 'file'])
            result['metadata']['entries'] = len(archive.members)
            result['metadata']['truncated'] = len(archive.members) > MAX_ROWS
        finally:
            archive.archive.close()
    else:
        wrappers = {'.gz': gzip.GzipFile, '.tgz': gzip.GzipFile, '.bz2': bz2.BZ2File, '.xz': lzma.LZMAFile}
        expanded = wrappers[suffix](fileobj=stream) if suffix in {'.gz', '.tgz'} else wrappers[suffix](stream) if suffix in wrappers else stream
        try:
            with tarfile.open(fileobj=_LimitedReader(expanded), mode='r|') as archive:
                for member in archive:
                    if len(rows) >= MAX_ROWS:
                        result['metadata']['truncated'] = True
                        break
                    if not _safe_member(member.name):
                        raise PreviewError('Archive contains an unsafe member name.')
                    rows.append([member.name[:MAX_CELL], str(member.size), 'folder' if member.isdir() else 'link' if member.issym() or member.islnk() else 'file'])
        finally:
            if expanded is not stream:
                expanded.close()
    result.update(kind='table', columns=['Name', 'Bytes', 'Type'], rows=rows)
    result['metadata']['preview_fidelity'] = 'archive listing; nothing is extracted'
    return result


def _media(stream, path, info, result):
    options = ['-protocol_whitelist', 'file,pipe', '-format_whitelist', MEDIA_FORMATS]
    data = _run(['ffprobe', '-v', 'error', *options, '-show_entries',
                 'format=duration,format_name:stream=codec_type,codec_name,width,height',
                 '-of', 'json', _fd_path(stream)], stream)
    media = json.loads(data)
    streams = media.get('streams', [])[:32]
    result.update(kind='media')
    result['metadata'].update(format=media.get('format', {}).get('format_name', ''),
                              duration=media.get('format', {}).get('duration', ''), streams=streams)
    video = next((item for item in streams if item.get('codec_type') == 'video'), None)
    if video:
        if int(video.get('width', 0)) * int(video.get('height', 0)) > MAX_PIXELS:
            raise PreviewError('Video exceeds the preview pixel limit.')
        with tempfile.TemporaryDirectory(prefix='video-', dir=_cache_root()) as temporary:
            output = str(Path(temporary) / 'poster.png')
            _run(['ffmpeg', '-v', 'error', '-nostdin', *options, '-i', _fd_path(stream),
                  '-frames:v', '1', '-vf', f'scale={MAX_EDGE}:{MAX_EDGE}:force_original_aspect_ratio=decrease',
                  '-threads', '1', output], stream)
            with Image.open(output) as image:
                result['image_path'] = _save_image(image, path, info, 0)
    return result


def _folder(path, result):
    rows = []
    fd = os.open(path, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0) | getattr(os, 'O_NOFOLLOW', 0))
    try:
        with os.scandir(fd) as entries:
            for entry in entries:
                if len(rows) >= MAX_ROWS:
                    result['metadata']['truncated'] = True
                    break
                try:
                    info = entry.stat(follow_symlinks=False)
                    kind = 'link' if stat.S_ISLNK(info.st_mode) else 'folder' if stat.S_ISDIR(info.st_mode) else 'file'
                    rows.append([entry.name[:MAX_CELL], kind, str(info.st_size)])
                except OSError:
                    rows.append([entry.name[:MAX_CELL], 'unavailable', ''])
    finally:
        os.close(fd)
    rows.sort(key=lambda row: row[0].casefold())
    result.update(kind='folder', columns=['Name', 'Type', 'Bytes'], rows=rows)
    return result


def _limit_payload(result):
    # Worker JSON remains below the UI transport cap even for four-byte text.
    if 'text' in result:
        encoded = result['text'].encode('utf-8')
        if len(encoded) > MAX_TEXT:
            result['metadata']['truncated'] = True
            result['text'] = encoded[:MAX_TEXT].decode('utf-8', 'ignore')
    if 'rows' in result:
        remaining, rows = 512 * 1024, []
        for row in result['rows'][:MAX_ROWS]:
            cells = []
            for cell in row[:MAX_COLUMNS]:
                encoded = str(cell)[:MAX_CELL].encode('utf-8')
                cells.append(encoded[:remaining].decode('utf-8', 'ignore'))
                remaining -= min(len(encoded), remaining)
            rows.append(cells)
            if remaining == 0:
                result['metadata']['truncated'] = True
                break
        result['rows'] = rows
    return result


def load_preview(path, page=0):
    """Read a bounded local preview. Errors are returned as a displayable dict."""
    result = {'kind': 'error', 'title': str(path)[:512], 'mime': 'application/octet-stream',
              'metadata': {}, 'size': 0, 'page': 0, 'page_count': 1}
    try:
        path = _local_path(path)
        info = path.lstat()
        mime = 'inode/directory' if stat.S_ISDIR(info.st_mode) else mimetypes.guess_type(str(path))[0] or 'application/octet-stream'
        result = _base(path, info, mime)
        category = category_for(path, mime)
        result['metadata']['category'] = category
        if stat.S_ISLNK(info.st_mode):
            result['metadata']['reason'] = 'Symbolic links are not followed by the preview worker.'
            return result
        if stat.S_ISDIR(info.st_mode):
            return _limit_payload(_folder(path, result))
        if not stat.S_ISREG(info.st_mode):
            result['metadata']['reason'] = 'Special files are shown as metadata only.'
            return result
        with _regular(path) as (stream, info):
            suffix = path.suffix.lower()
            if suffix in OFFICE:
                result = _office(stream, info, result, suffix, page)
            elif category == 'pdf':
                result = _pdf(stream, path, info, result, page)
            elif category == 'image' and suffix != '.svg':
                result = _bounded_image(stream, path, info, result)
            elif category in {'text', 'code'} or suffix in {'.csv', '.tsv', '.svg'}:
                result = _text(stream, result, suffix)
            elif category == 'archive' and suffix not in {'.7z', '.rar'}:
                result = _archive(stream, info, result, suffix)
            elif category in {'audio', 'video'}:
                result = _media(stream, path, info, result)
            elif category == 'unknown':
                data = stream.read(min(MAX_READ, info.st_size))
                if data.startswith((b'\x7fELF', b'MZ', b'\xcf\xfa\xed\xfe', b'\xfe\xed\xfa\xcf')):
                    result['metadata']['category'] = 'executable'
                    result['metadata']['reason'] = 'Executable files are shown as metadata only.'
                else:
                    try:
                        value = _decode(data)
                        if value and '\ufffd' not in value:
                            result.update(kind='text', text=value[:MAX_TEXT])
                    except PreviewError:
                        pass
            else:
                result['metadata']['reason'] = 'This format is shown as metadata only.'
        return _limit_payload(result)
    except (OSError, ValueError, EOFError, RuntimeError, NotImplementedError, struct.error, ET.ParseError, zipfile.BadZipFile, tarfile.TarError,
            Image.DecompressionBombError, Image.DecompressionBombWarning) as error:
        result.update(kind='error')
        result['metadata']['error'] = str(error)[:512]
        result.pop('image_path', None)
        return result


@lru_cache(maxsize=8)
def _font(size):
    paths = ['/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc',
             '/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf',
             '/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf']
    for path in paths:
        if Path(path).is_file():
            return ImageFont.truetype(path, size)
    return ImageFont.load_default()


def make_thumbnail(path, output, size=256):
    """Silently write a bounded actual-content thumbnail atomically; return bool.

    The output directory is supplied/owned by the caller. Unsupported files
    return False so the native renderer retains its real type icon.
    """
    try:
        size = max(32, min(256, int(size)))
        preview = load_preview(path)
        if preview['kind'] in {'unsupported', 'error'}:
            return False
        image = Image.new('RGBA', (size, size), (0, 0, 0, 0))
        draw = ImageDraw.Draw(image)
        margin, radius = max(2, size // 25), max(4, size // 18)
        box = (margin, margin, size - margin - 1, size - margin - 1)
        draw.rounded_rectangle(box, radius=radius, fill=(242, 248, 255, 215), outline=(255, 255, 255, 230), width=1)
        inner = (margin + 4, margin + 4, size - margin - 4, max(margin + 6, size - margin - 25))
        if preview.get('image_path'):
            with Image.open(preview['image_path']) as source:
                if source.width * source.height > MAX_PIXELS:
                    return False
                content = ImageOps.contain(source.convert('RGBA'), (inner[2] - inner[0], inner[3] - inner[1]), Image.Resampling.LANCZOS)
                image.alpha_composite(content, (inner[0] + (inner[2] - inner[0] - content.width) // 2, inner[1]))
        else:
            value = preview.get('text', '')
            if preview['kind'] in {'table', 'folder'}:
                value = '\n'.join('  '.join(str(cell) for cell in row[:4]) for row in ([preview.get('columns', [])] + preview.get('rows', [])[:12]))
            elif preview['kind'] == 'media':
                metadata = preview['metadata']
                value = '\n'.join(filter(None, [preview['title'], metadata.get('format', ''), str(metadata.get('duration', ''))]))
            font_size = max(8, size // 24)
            font = _font(font_size)
            y = inner[1]
            for line in value.splitlines()[:24]:
                if y + font_size + 3 > inner[3]:
                    break
                line = line[:100]
                while line and draw.textlength(line, font=font) > inner[2] - inner[0]:
                    line = line[:-1]
                draw.text((inner[0], y), line, fill=(39, 52, 67, 245), font=font)
                y += font_size + 3
        category = preview['metadata'].get('category', 'file')
        chip = Path(os.fspath(path)).suffix.lstrip('.').upper()[:8] or category.upper()[:8]
        draw.rounded_rectangle((margin + 5, size - margin - 21, min(size - margin - 4, margin + 15 + len(chip) * 7), size - margin - 5), radius=5, fill=(221, 236, 251, 235))
        draw.text((margin + 9, size - margin - 20), chip, font=_font(10), fill=(47, 84, 123, 255))
        _atomic_png(image, output)
        return True
    except (OSError, ValueError, KeyError, TypeError):
        return False
