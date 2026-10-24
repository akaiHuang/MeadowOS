"""Actual local data and bounded parser checks; no GUI or file execution."""
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import signal
import sys
import time
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from PIL import Image

SOURCE = Path(__file__).resolve().parents[1] / 'file_preview.py'
spec = importlib.util.spec_from_file_location('file_preview', SOURCE)
preview = importlib.util.module_from_spec(spec)
spec.loader.exec_module(preview)

W = 'http://schemas.openxmlformats.org/wordprocessingml/2006/main'
A = 'http://schemas.openxmlformats.org/drawingml/2006/main'
P = 'http://schemas.openxmlformats.org/presentationml/2006/main'
R = 'http://schemas.openxmlformats.org/officeDocument/2006/relationships'
S = 'http://schemas.openxmlformats.org/spreadsheetml/2006/main'
REL = 'http://schemas.openxmlformats.org/package/2006/relationships'
ODF = ('xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" '
       'xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" '
       'xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" '
       'xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"')


class PreviewTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='meadow-preview-test-')
        self.root = Path(self.temporary.name)
        self.environment = patch.dict(os.environ, {'XDG_CACHE_HOME': str(self.root / 'cache')})
        self.environment.start()

    def tearDown(self):
        self.environment.stop()
        self.temporary.cleanup()

    def file(self, name, data):
        target = self.root / name
        target.write_bytes(data.encode() if isinstance(data, str) else data)
        return target

    def office(self, name, members):
        target = self.root / name
        with zipfile.ZipFile(target, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
            for path, data in members.items():
                archive.writestr(path, data)
        return target

    def assert_kind(self, path, kind, page=0):
        value = preview.load_preview(path, page)
        self.assertEqual(value['kind'], kind, value)
        for key in ('title', 'mime', 'metadata', 'size', 'page', 'page_count'):
            self.assertIn(key, value)
        return value

    def test_plain_text_actual_change_is_not_a_cached_fake_preview(self):
        target = self.file('notes.txt', '第一版\nBudget 42')
        self.assertEqual(self.assert_kind(target, 'text')['text'], '第一版\nBudget 42')
        target.write_text('第二版\nBudget 84')
        self.assertEqual(self.assert_kind(target, 'text')['text'], '第二版\nBudget 84')

    def test_html_and_code_are_text_without_execution(self):
        target = self.file('test.html', '<script>fetch("https://example.invalid/")</script>')
        with patch.object(preview.subprocess, 'Popen', side_effect=AssertionError('No process allowed')):
            value = self.assert_kind(target, 'text')
        self.assertEqual(value['text'], target.read_text())
        self.assertEqual(preview.category_for('script.py'), 'code')

    def test_text_and_table_output_are_bounded(self):
        target = self.file('large.txt', 'actual\n' * 60000)
        value = self.assert_kind(target, 'text')
        self.assertLessEqual(len(value['text']), preview.MAX_TEXT)
        self.assertTrue(value['metadata']['truncated'])
        csv_file = self.file('rows.csv', 'Name,Cost\n' + ('真实資料,42\n' * 150))
        table = self.assert_kind(csv_file, 'table')
        self.assertEqual(table['columns'], ['Name', 'Cost'])
        self.assertEqual(table['rows'][0], ['真实資料', '42'])
        self.assertEqual(len(table['rows']), preview.MAX_ROWS)
        self.assertLess(len(json.dumps(table)), 1024 * 1024)

    def test_symlink_fifo_and_uri_do_not_read_or_start_decoders(self):
        target = self.file('secret.txt', 'Private fixture')
        link = self.root / 'alias.txt'
        link.symlink_to(target)
        fifo = self.root / 'fifo'
        os.mkfifo(fifo)
        with patch.object(preview.subprocess, 'Popen', side_effect=AssertionError('No process allowed')):
            self.assert_kind(link, 'unsupported')
            self.assert_kind(fifo, 'unsupported')
            self.assert_kind('https://example.invalid/file.pdf', 'error')
            self.assert_kind('file:///tmp/file.pdf', 'error')

    def test_unknown_binary_and_executable_are_metadata_only(self):
        self.assert_kind(self.file('unknown.data', b'\x00\xff\x04\x00\x01'), 'unsupported')
        self.assert_kind(self.file('elf-no-extension', b'\x7fELF\x00' + b'a' * 32), 'unsupported')
        self.assert_kind(self.file('program.exe', b'MZ1234'), 'unsupported')
        self.assert_kind(self.file('old.doc', b'\xd0\xcf\x11\xe0'), 'unsupported')

    def test_image_preview_is_owned_bounded_png_and_changes_with_content(self):
        target = self.root / 'picture.png'
        Image.new('RGB', (2200, 900), 'red').save(target)
        value = self.assert_kind(target, 'image', page=999)
        self.assertNotEqual(value['image_path'], str(target))
        self.assertIn(str(self.root / 'cache'), value['image_path'])
        with Image.open(value['image_path']) as image:
            self.assertLessEqual(max(image.size), preview.MAX_EDGE)
            self.assertEqual(image.format, 'PNG')
            self.assertEqual(image.getpixel((10, 10))[:3], (255, 0, 0))
        Image.new('RGB', (60, 40), 'blue').save(target)
        changed = self.assert_kind(target, 'image')
        self.assertNotEqual(value['image_path'], changed['image_path'])
        with Image.open(changed['image_path']) as image:
            self.assertEqual(image.getpixel((10, 10))[:3], (0, 0, 255))

    def test_image_pixel_limit_precedes_pixel_decode(self):
        target = self.root / 'oversized.png'
        Image.new('RGB', (300, 300), 'red').save(target)
        with patch.object(preview, 'MAX_PIXELS', 50000):
            self.assert_kind(target, 'error')

    def test_thumbnail_actual_text_changes_and_output_is_atomic_rgba(self):
        target = self.file('notes.md', 'Morning report\nReal first sentence')
        output = self.root / 'thumb.png'
        self.assertTrue(preview.make_thumbnail(target, output, 999))
        first = output.read_bytes()
        with Image.open(output) as image:
            self.assertEqual(image.size, (256, 256))
            self.assertEqual(image.mode, 'RGBA')
        target.write_text('New report\nChanged actual content')
        self.assertTrue(preview.make_thumbnail(target, output))
        self.assertNotEqual(first, output.read_bytes())
        self.assertEqual(list(self.root.glob('.meadow-preview-*')), [])
        link = self.root / 'bad-output.png'
        link.symlink_to(target)
        self.assertFalse(preview.make_thumbnail(target, link))
        self.assertEqual(target.read_text(), 'New report\nChanged actual content')

    def test_docx_actual_paragraphs(self):
        target = self.office('document.docx', {'word/document.xml': f'<w:document xmlns:w="{W}"><w:body><w:p><w:r><w:t>Actual title</w:t></w:r></w:p><w:p><w:r><w:t>實際內容</w:t></w:r></w:p></w:body></w:document>'})
        value = self.assert_kind(target, 'text')
        self.assertEqual(value['text'], 'Actual title\n實際內容')
        self.assertIn('layout is not rendered', value['metadata']['preview_fidelity'])

    def test_pptx_honors_relationship_order_and_page_bounds(self):
        target = self.office('slides.pptx', {
            'ppt/presentation.xml': f'<p:presentation xmlns:p="{P}" xmlns:r="{R}"><p:sldIdLst><p:sldId id="1" r:id="r2"/><p:sldId id="2" r:id="r1"/></p:sldIdLst></p:presentation>',
            'ppt/_rels/presentation.xml.rels': f'<Relationships xmlns="{REL}"><Relationship Id="r1" Target="slides/slide1.xml"/><Relationship Id="r2" Target="slides/slide2.xml"/></Relationships>',
            'ppt/slides/slide1.xml': f'<p:sld xmlns:p="{P}" xmlns:a="{A}"><a:p><a:r><a:t>Second actual slide</a:t></a:r></a:p></p:sld>',
            'ppt/slides/slide2.xml': f'<p:sld xmlns:p="{P}" xmlns:a="{A}"><a:p><a:r><a:t>First actual slide</a:t></a:r></a:p></p:sld>'})
        self.assertEqual(self.assert_kind(target, 'text', -5)['text'], 'First actual slide')
        last = self.assert_kind(target, 'text', 999)
        self.assertEqual((last['page'], last['page_count']), (1, 2))
        self.assertEqual(last['text'], 'Second actual slide')

    def test_xlsx_shared_strings_coordinates_and_cached_formula(self):
        target = self.office('table.xlsx', {
            'xl/workbook.xml': f'<workbook xmlns="{S}" xmlns:r="{R}"><sheets><sheet name="Budget" r:id="r1"/></sheets></workbook>',
            'xl/_rels/workbook.xml.rels': f'<Relationships xmlns="{REL}"><Relationship Id="r1" Target="worksheets/sheet1.xml"/></Relationships>',
            'xl/sharedStrings.xml': f'<sst xmlns="{S}"><si><t>Actual project</t></si></sst>',
            'xl/worksheets/sheet1.xml': f'<worksheet xmlns="{S}"><sheetData><row r="4"><c r="B4" t="s"><v>0</v></c><c r="D4"><f>2*21</f><v>42</v></c></row></sheetData></worksheet>'})
        value = self.assert_kind(target, 'table', 99)
        self.assertEqual(value['columns'], ['B', 'D'])
        self.assertEqual(value['rows'], [['Actual project', '42']])
        self.assertEqual(value['metadata']['row_numbers'], ['4'])
        self.assertFalse(value['metadata']['formulas_evaluated'])

    def test_odt_odp_and_ods_actual_content_and_repeat_limits(self):
        document = self.office('document.odt', {'content.xml': f'<office:document-content {ODF}><text:h>Actual heading</text:h><text:p>Actual paragraph</text:p></office:document-content>'})
        self.assertEqual(self.assert_kind(document, 'text')['text'], 'Actual heading\nActual paragraph')
        slides = self.office('slides.odp', {'content.xml': f'<office:document-content {ODF}><draw:page><text:p>First</text:p></draw:page><draw:page><text:p>Second</text:p></draw:page></office:document-content>'})
        self.assertEqual(self.assert_kind(slides, 'text', 1)['text'], 'Second')
        sheet = self.office('sheet.ods', {'content.xml': f'<office:document-content {ODF}><table:table table:name="Actual sheet"><table:table-row table:number-rows-repeated="999999999"><table:table-cell table:number-columns-repeated="999999999"><text:p>Cell</text:p></table:table-cell></table:table-row></table:table></office:document-content>'})
        value = self.assert_kind(sheet, 'table')
        self.assertEqual(len(value['rows']), preview.MAX_ROWS)
        self.assertEqual(len(value['columns']), preview.MAX_COLUMNS)
        self.assertEqual(value['rows'][0][0], 'Cell')

    def test_zip_traversal_symlinks_and_entity_bombs_are_rejected(self):
        escape = self.office('escape.zip', {'../outside.txt': 'Do not extract'})
        self.assert_kind(escape, 'error')
        self.assertFalse((self.root.parent / 'outside.txt').exists())
        link = self.root / 'link.zip'
        with zipfile.ZipFile(link, 'w') as archive:
            member = zipfile.ZipInfo('alias')
            member.external_attr = (0o120777 << 16)
            archive.writestr(member, '/etc/passwd')
        self.assert_kind(link, 'error')
        entity = self.office('entity.docx', {'word/document.xml': '<!DOCTYPE test [<!ENTITY x "text"><!ENTITY y "&x;&x;">]><test>&y;</test>'})
        self.assert_kind(entity, 'error')
        encoded = self.office('utf16.docx', {'word/document.xml': '<!DOCTYPE test [<!ENTITY x "bad">]><test>&x;</test>'.encode('utf-16')})
        self.assert_kind(encoded, 'error')

    def test_zip_expansion_node_and_member_count_limits(self):
        large = self.office('large.docx', {'word/document.xml': 'x' * (preview.MAX_EXPANDED + 1)})
        self.assert_kind(large, 'error')
        many = self.office('many.zip', {f'{n}.txt': 'actual' for n in range(8)})
        with patch.object(preview, 'MAX_ENTRIES', 4):
            self.assert_kind(many, 'error')
        nested = self.office('nested.docx', {'word/document.xml': '<a>' * 70 + 'actual' + '</a>' * 70})
        self.assert_kind(nested, 'error')

    def test_zip_and_tar_list_actual_entries_without_extracting(self):
        zipped = self.office('actual.zip', {'a.txt': 'alpha', 'dir/b.txt': 'beta'})
        table = self.assert_kind(zipped, 'table')
        self.assertEqual(table['rows'][0], ['a.txt', '5', 'file'])
        target = self.root / 'actual.tar.gz'
        with tarfile.open(target, 'w:gz') as archive:
            member = tarfile.TarInfo('actual.txt')
            data = b'actual content'
            member.size = len(data)
            archive.addfile(member, io.BytesIO(data))
        table = self.assert_kind(target, 'table')
        self.assertEqual(table['rows'], [['actual.txt', '14', 'file']])
        self.assertFalse((self.root / 'actual.txt').exists())

    def test_folder_entries_are_bounded_and_symlinks_are_not_traversed(self):
        directory = self.root / 'folder'
        directory.mkdir()
        (directory / 'actual.txt').write_text('data')
        (directory / 'alias').symlink_to(self.root)
        value = self.assert_kind(directory, 'folder')
        self.assertIn(['actual.txt', 'file', '4'], value['rows'])
        self.assertEqual(next(row[1] for row in value['rows'] if row[0] == 'alias'), 'link')

    def test_unicode_payload_and_malformed_archive_names_are_bounded(self):
        value = self.assert_kind(self.file('unicode.txt', '😀' * 100000), 'text')
        self.assertLessEqual(len(value['text'].encode('utf-8')), preview.MAX_TEXT)
        payload = {'kind': 'table', 'metadata': {}, 'rows': [['😀' * 512] * 12 for _ in range(100)]}
        bounded = preview._limit_payload(payload)
        self.assertLess(len(json.dumps(bounded, ensure_ascii=False).encode()), 1024 * 1024)
        self.assertTrue(bounded['metadata']['truncated'])
        self.assert_kind(self.office('dot.zip', {'.': 'actual'}), 'error')
        self.assert_kind(self.office('dot-slash.zip', {'./': 'actual'}), 'error')

    def test_compressed_tar_expansion_limit_is_enforced(self):
        target = self.root / 'expansion.tar.gz'
        with tarfile.open(target, 'w:gz') as archive:
            data = b'0' * 200000
            member = tarfile.TarInfo('large.bin')
            member.size = len(data)
            archive.addfile(member, io.BytesIO(data))
        with patch.object(preview, 'MAX_EXPANDED', 65536):
            self.assert_kind(target, 'error')

    @unittest.skipUnless(sys.platform.startswith('linux'), 'Linux parent-death signal')
    def test_decoder_dies_when_owned_worker_is_cancelled(self):
        marker = self.root / 'decoder.pid'
        source = self.file('source.txt', 'actual')
        decoder = f"import os,time;open({str(marker)!r},'w').write(str(os.getpid()));time.sleep(30)"
        worker = ("import importlib.util;"
                  f"s=importlib.util.spec_from_file_location('preview',{str(SOURCE)!r});"
                  "m=importlib.util.module_from_spec(s);s.loader.exec_module(m);"
                  f"f=open({str(source)!r},'rb');m._run([{sys.executable!r},'-c',{decoder!r}],f)")
        process = subprocess.Popen([sys.executable, '-c', worker], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        child = None
        def active(pid):
            state = Path(f'/proc/{pid}/stat')
            if not state.exists():
                return False
            try:
                return state.read_text().split(') ', 1)[1].split()[0] not in {'Z', 'X'}
            except FileNotFoundError:
                return False
        try:
            deadline = time.monotonic() + 3
            while not marker.exists() and time.monotonic() < deadline:
                time.sleep(.02)
            self.assertTrue(marker.exists(), 'decoder started and wrote its owned PID')
            child = int(marker.read_text())
            self.assertTrue(active(child))
            process.kill()
            process.wait(timeout=2)
            deadline = time.monotonic() + 2
            while active(child) and time.monotonic() < deadline:
                time.sleep(.02)
            self.assertFalse(active(child), 'cancelled worker left a live decoder')
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=2)
            if child and active(child):
                os.kill(child, signal.SIGKILL)

    @unittest.skipUnless(shutil.which('pdfinfo') and shutil.which('pdftoppm'), 'Poppler unavailable')
    def test_actual_pdf_render_is_bounded_and_page_clamped(self):
        target = self.root / 'actual.pdf'
        first, second = Image.new('RGB', (300, 200), 'red'), Image.new('RGB', (300, 200), 'blue')
        first.save(target, save_all=True, append_images=[second], resolution=72)
        value = self.assert_kind(target, 'image', 999)
        self.assertEqual((value['page'], value['page_count']), (1, 2))
        with Image.open(value['image_path']) as rendered:
            self.assertLessEqual(max(rendered.size), preview.MAX_EDGE)
            red, green, blue, *_ = rendered.getpixel((100, 100))
            self.assertGreater(blue, red + 100)

    @unittest.skipUnless(shutil.which('ffmpeg') and shutil.which('ffprobe'), 'FFmpeg unavailable')
    def test_actual_video_poster_and_audio_metadata(self):
        target = self.root / 'actual.mp4'
        subprocess.run(['ffmpeg', '-v', 'error', '-f', 'lavfi', '-i', 'color=c=blue:s=160x100:d=0.2',
                        '-pix_fmt', 'yuv420p', str(target)], check=True, timeout=8)
        value = self.assert_kind(target, 'media')
        self.assertTrue(value.get('image_path'))
        with Image.open(value['image_path']) as rendered:
            self.assertLessEqual(max(rendered.size), preview.MAX_EDGE)
            red, green, blue, *_ = rendered.getpixel((20, 20))
            self.assertGreater(blue, red + 100)
        audio = self.root / 'actual.wav'
        subprocess.run(['ffmpeg', '-v', 'error', '-f', 'lavfi', '-i', 'sine=frequency=440:duration=0.1', str(audio)], check=True, timeout=8)
        value = self.assert_kind(audio, 'media')
        self.assertFalse(value.get('image_path'))
        self.assertEqual(value['metadata']['streams'][0]['codec_type'], 'audio')


if __name__ == '__main__':
    unittest.main()
