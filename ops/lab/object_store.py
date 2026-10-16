#!/usr/bin/env python3
"""Explicit, local kind-only S3 provisioning. No public endpoint or FileStorage.

Run against an unused local lab; validate_modules.py uses a disposable namespace.
Credentials are random and kept in the private lab state, never command output.
The single SeaweedFS/PVC store is shared by Asset replicas but is NOT HA storage.
"""
import hashlib
import json
from pathlib import Path
import re
import secrets

import app
from infra import HERE, STATE, apply, kube


def configuration(state):
    path = Path(state)/'asset-s3.json'
    if not path.exists():
        config = {'endpoint':'http://asset-store.human-worth.svc.cluster.local:8333',
                  'bucket':'human-worth-assets', 'region':'us-east-1',
                  'access_key':secrets.token_hex(16), 'secret_key':secrets.token_hex(32),
                  'force_path_style':True}
        app.private_file(path, json.dumps(config)+'\n')
    return app.object_storage_config(state)


def install(config):
    # Filer's local management API creates the private bucket, not an anonymous
    # external client. Runtime S3 credentials are scoped to exactly this bucket.
    auth = json.dumps({'identities':[{'name':'asset-runtime',
                    'credentials':[{'accessKey':config['access_key'], 'secretKey':config['secret_key']}],
                    'actions':[action+':'+config['bucket'] for action in ['Read','Write','List','Tagging']]}]})
    app.secret('asset-store', {'s3.json':auth})
    text = (HERE/'asset-store.yaml').read_text()
    # Rotate the storage Pod as well as Asset if its credentials change.
    text = text.replace('metadata: {labels: {app: asset-store}}',
                        'metadata: {labels: {app: asset-store}, annotations: {human-worth.io/config: "'+hashlib.sha256(auth.encode()).hexdigest()+'"}}')
    apply(text)
    kube('-n','human-worth','rollout','status','statefulset/asset-store','--timeout=180s')
    # `weed shell` reports command errors on stdout and exits zero, so a successful
    # exec alone cannot prove bucket creation. Create is safe to repeat; list then
    # confirms that the exact private bucket exists. The IAM owner is required for
    # these non-admin runtime credentials.
    command = ('s3.bucket.create -name '+config['bucket']+' -owner asset-runtime\n'
               's3.bucket.list\n')
    output = kube('-n','human-worth','exec','-i','asset-store-0','--','weed','shell',
                  '-filer=127.0.0.1:8888',input=command,capture=True).stdout
    # Even piped input prints the interactive "> " prompt before the first
    # bucket. Match that record and its IAM owner, not a create success message.
    record = (r'^(?:>[ \t]*)?[ \t]*'+re.escape(config['bucket'])+
              r'\t[^\r\n]*\towner:"asset-runtime"[ \t]*$')
    if not re.search(record, output, re.MULTILINE):
        raise RuntimeError('Asset S3 bucket provisioning did not confirm the private bucket')
    print('Private shared lab S3 store provisioned (not production HA)',flush=True)


def main():
    STATE.mkdir(parents=True,mode=0o700,exist_ok=True)
    if (STATE/'release-revision').exists():
        raise RuntimeError('This lab serves approved releases; use validate_modules.py instead of altering its infrastructure')
    config=configuration(STATE)
    install(config)


if __name__=='__main__':main()
