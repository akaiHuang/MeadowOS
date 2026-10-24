"""Offline native authenticated-upload and compact collection send contract."""
import base64
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from agent_client import AgentClient, AgentError

PNG = base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl6XWQAAAAASUVORK5CYII=')


class UploadResponse(io.BytesIO):
    status = 200


class AgentAttachmentTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name).resolve()
        self.client = AgentClient(workspace=root/'workspace', state_root=root/'agent',
            persistence_path=root/'state'/'conversation.json')
        self.client.session = {'sessionId': 'live-session', 'chatId': 'durable-chat'}
        self.client._request = Mock(return_value={'disposition': 'accepted'})
        self.client._release = Mock()
        self.uploads = []

        def upload(method, path, *, headers, raw_body):
            original = raw_body.read()
            self.uploads.append((method, path, headers, original))
            image_id = path.rsplit('/', 1)[1]
            collection = path.split('/attachment-collections/')[1].split('/')[0]
            value = {'id': image_id, 'collection_id': collection,
                'sha256': headers['X-Meadow-Attachment-SHA256'], 'size': len(original)}
            return object(), UploadResponse(json.dumps(value).encode())
        self.client._open = Mock(side_effect=upload)

    def stage(self, name='reference.png'):
        # Use a complete generated PNG; avoids relying on a decoder's handling
        # of historically malformed tiny PNG fixture CRCs.
        import struct, zlib
        def chunk(kind, data):
            return struct.pack('>I', len(data))+kind+data+struct.pack('>I', zlib.crc32(kind+data)&0xffffffff)
        data = b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR', struct.pack('>IIBBBBB',1,1,8,2,0,0,0))+chunk(b'IDAT',zlib.compress(b'\x00\x20\x40\x60'))+chunk(b'IEND',b'')
        return self.client.stage_attachment_bytes(data, name, 'image/png')

    def test_original_stream_and_opaque_collection_preserve_exact_task(self):
        item = self.stage('中文 reference.png')
        request_id = str(uuid.uuid4())
        events = []
        result = self.client.send('  Exact task\nKeep this.  ', attachments=[item],
            request_id=request_id, on_attachment=events.append)
        self.assertEqual(result['disposition'], 'accepted')
        self.assertEqual(self.uploads[0][0], 'PUT')
        self.assertEqual(self.uploads[0][3], Path(item['path']).read_bytes())
        self.assertEqual(self.uploads[0][2]['Content-Length'], str(item['size']))
        self.assertEqual([event['state'] for event in events], ['uploading', 'uploaded'])
        body = self.client._request.call_args.args[2]
        self.assertEqual(body['prompt'][0], {'type':'text','text':'  Exact task\nKeep this.  '})
        self.assertEqual(body['prompt'][1]['type'], 'attachment_collection')
        self.assertEqual(body['prompt'][1]['collection_id'], request_id)
        self.assertEqual(body['prompt'][1]['expected_count'], 1)
        self.assertEqual(len(body['prompt'][1]['manifest_sha256']), 64)
        self.assertNotIn('base64', json.dumps(body))
        retained = self.client.attachment_cache.get(item['id'])
        self.assertEqual(retained['state'], 'submitted')
        self.assertEqual(retained['ref']['collection_id'], request_id)

    def test_attachment_only_send_and_deliberate_retry_reuse_original_receipt(self):
        item = self.stage()
        request_id = str(uuid.uuid4())
        self.client.send('', attachments=[item], request_id=request_id)
        self.client.send('', attachments=[item], request_id=request_id)
        self.assertEqual(len(self.uploads), 1)
        prompt = self.client._request.call_args.args[2]['prompt']
        self.assertEqual(len(prompt), 1)
        self.assertEqual(prompt[0]['collection_id'], request_id)

    def test_forty_images_cross_individually_and_prompt_size_stays_constant(self):
        items = [self.stage(str(index)+'.png') for index in range(40)]
        self.client.send('Compare these', attachments=items)
        self.assertEqual(len(self.uploads), 40)
        body = self.client._request.call_args.args[2]
        self.assertLess(len(json.dumps(body)), 400)
        self.assertEqual(len(body['prompt']), 2)

    def test_individual_failures_retain_original_and_do_not_admit_partial_task(self):
        items = [self.stage('one.png'), self.stage('two.png')]
        original_upload = self.client._open.side_effect
        def fail_second(*args, **kwargs):
            if len(self.uploads) == 1:
                raise AgentError('rejected image', code='attachment_image_invalid')
            return original_upload(*args, **kwargs)
        self.client._open.side_effect = fail_second
        with self.assertRaises(AgentError) as caught:
            self.client.send('Task', attachments=items)
        self.assertEqual(caught.exception.code, 'attachment_upload_incomplete')
        self.assertEqual(len(caught.exception.payload['failures']), 1)
        self.assertEqual(self.client._request.call_count, 1)  # collection only
        self.assertEqual(self.client.attachment_cache.get(items[0]['id'])['state'], 'uploaded')
        self.assertEqual(self.client.attachment_cache.get(items[1]['id'])['state'], 'failed')
        self.assertTrue(Path(items[1]['path']).exists())

    def test_unknown_queue_receipt_keeps_sources_submitted_for_explicit_recovery(self):
        item = self.stage()
        self.client._request.side_effect = [{}, AgentError('unknown admission',code='response_interrupted')]
        with self.assertRaises(AgentError) as caught:
            self.client.send('Task', attachments=[item])
        self.assertEqual(caught.exception.code, 'response_interrupted')
        self.assertEqual(self.client.attachment_cache.get(item['id'])['state'], 'submitted')
        self.assertEqual(self.client._request.call_count, 2)


if __name__ == '__main__':
    unittest.main()
