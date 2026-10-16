import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]/'ops/lab'))
import infra


class LabInfraTest(unittest.TestCase):
    def test_proxy_skips_ipam_entries_without_ipv4_gateway(self):
        for entries in [
            [{'Subnet':'fc00:f853:ccd:e793::/64'},
             {'Subnet':'172.18.0.0/16','Gateway':'172.18.0.1'}],
            [{'Gateway':None}, {'Gateway':''}, {'Gateway':'fc00::1'},
             {'Gateway':'172.18.0.1'}],
            [{'Gateway':'172.18.0.1'}],
        ]:
            with self.subTest(entries=entries), tempfile.TemporaryDirectory() as directory:
                network=json.dumps([{'IPAM':{'Config':entries}}])
                def run(*args, **kwargs):
                    return subprocess.CompletedProcess(args, 0, stdout=network if 'inspect' in args else '')
                with patch.dict(infra.os.environ, {'HTTPS_PROXY':'http://127.0.0.1:7890'}, clear=True), patch.object(infra,'STATE',Path(directory)), patch.object(infra,'run',side_effect=run) as command, patch.object(infra.subprocess,'run',return_value=subprocess.CompletedProcess([],1,stdout='')):
                    self.assertEqual(infra.host_proxy(['lab-worker']), 'http://172.18.0.1:7890')
                self.assertEqual(json.loads((Path(directory)/'egress-parent.json').read_text()), {'host':'172.18.0.1','port':7890})
                dropin=next(call.kwargs['input'] for call in command.call_args_list if 'tee' in call.args)
                self.assertIn('Environment="HTTPS_PROXY=http://172.18.0.1:7890"',dropin)

    def test_proxy_without_ipv4_gateway_fails_before_node_changes(self):
        for entries in [[], [{}], [{'Gateway':None}], [{'Gateway':'fc00::1'}], [{'Gateway':'not-an-address'}]]:
            with self.subTest(entries=entries), tempfile.TemporaryDirectory() as directory:
                network=json.dumps([{'IPAM':{'Config':entries}}])
                with patch.dict(infra.os.environ, {'HTTPS_PROXY':'http://127.0.0.1:7890'}, clear=True), patch.object(infra,'STATE',Path(directory)), patch.object(infra,'run',return_value=subprocess.CompletedProcess([],0,stdout=network)) as command, patch.object(infra.subprocess,'run') as inspect_node:
                    with self.assertRaisesRegex(RuntimeError,'IPv4 gateway'):
                        infra.host_proxy(['lab-worker'])
                    self.assertEqual(command.call_count,1)
                    inspect_node.assert_not_called()
                    self.assertFalse((Path(directory)/'egress-parent.json').exists())
