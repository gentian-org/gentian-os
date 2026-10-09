#!/usr/bin/env python3
# =============================================================================
# scripts/tests/mail_edge_lab_build.py — the files of test-mail-edge-lab.sh
# =============================================================================
"""Turn the rendered manifests into the files the three containers mount.
Nothing is written by hand that a chart renders: the HAProxy configuration,
dovecot.conf, Postfix's environment and its edge-listeners script are the
charts' own output. What is supplied here is what the operator and
cert-manager supply on a cluster: the maps, the passwd-files, a certificate."""
import os, sys, yaml, pathlib, subprocess
render = pathlib.Path(sys.argv[1]); out = pathlib.Path(sys.argv[2]); out.mkdir(exist_ok=True)
def docs(f): return [d for d in yaml.safe_load_all(open(f)) if d]
def cm(dd, name): return next(d for d in dd if d['kind']=='ConfigMap' and d['metadata']['name']==name)['data']
pods_cidr = os.environ['PODS_CIDR']

up = docs(render/'upstream.yaml'); pf = docs(render/'postfix.yaml'); dv = docs(render/'dovecot.yaml'); ed = docs(render/'edge.yaml')
# Postfix: the upstream chart's ConfigMap is its envFrom.
env = dict(cm(up, 'postfix-dev')); tls = env.pop('_enable_tls.sh')
# The two keys the StatefulSet takes from the operator's ConfigMap.
env['POSTFIX_mynetworks'] = '127.0.0.0/8,' + pods_cidr
env['ALLOWED_SENDER_DOMAINS'] = 'lab.test tenant.lab.test'
(out/'postfix.env').write_text(''.join(f'{k}={v}\n' for k, v in env.items()))
d = out/'postfix'; (d/'init').mkdir(parents=True, exist_ok=True)
(d/'init/_enable_tls.sh').write_text(tls); os.chmod(d/'init/_enable_tls.sh', 0o755)
(d/'init/gentian-edge-listeners.sh').write_text(cm(pf, 'postfix-dev-edge-listeners')['edge-listeners.sh'])
os.chmod(d/'init/gentian-edge-listeners.sh', 0o644)
(d/'gentian').mkdir(exist_ok=True)
for k, v in cm(pf, 'postfix-dev-internal-headers').items(): (d/'gentian'/k).write_text(v)
# The maps, as syncPostfixVirtualMailboxMaps writes them for one tenant.
(d/'kernel-maps').mkdir(exist_ok=True)
doms = 'lab.test OK\ntenant.lab.test OK\n'
for k, v in {'virtual_mailbox_domains': doms, 'sender_access': doms,
             'virtual_mailbox_maps': '@lab.test lab.test/\n@tenant.lab.test tenant.lab.test/\n', 'virtual_alias': ''}.items():
    (d/'kernel-maps'/k).write_text(v)
(d/'tenant-keys').mkdir(exist_ok=True); (d/'dkim-keys').mkdir(exist_ok=True)

# Dovecot: the chart's conf, and the passwd-files in the operator's shape.
d = out/'dovecot'; (d/'gentian').mkdir(parents=True, exist_ok=True)
for k, v in cm(dv, 'dovecot-dev-config').items(): (d/'gentian'/k).write_text(v)
(d/'apppw').mkdir(exist_ok=True); (d/'realms').mkdir(exist_ok=True); (d/'mail').mkdir(exist_ok=True)
def passwd(app, tenant, user, pw):
    h = subprocess.check_output(['docker','run','--rm','--entrypoint','doveadm','dovecot/dovecot:2.3.21','pw','-s','ARGON2ID','-p',pw]).decode().strip()
    f = f'{app}-{tenant}'
    (d/'apppw'/f'{f}.users').write_text(f'{user}:{h}\n')
    (d/'apppw'/f'{f}.conf').write_text("passdb {\n  driver = passwd-file\n  args = /etc/dovecot/apppw/%s.users\n  result_failure = continue\n  result_internalfail = continue\n}\n" % f)
passwd('nextcloud-mail', 'tenant', 'ada@tenant.lab.test', 'imap-secret')
passwd('tenant-apps', 'tenant', 'smtp-tenant', 'smtp-secret')
# The edge: the chart's haproxy.cfg.
d = out/'edge'; d.mkdir(exist_ok=True)
(d/'haproxy.cfg').write_text(cm(ed, 'mail-edge-dev-config')['haproxy.cfg'])
dep = next(x for x in ed if x['kind']=='Deployment')['spec']['template']['spec']['containers'][0]
(out/'edge.image').write_text(dep['image']); (out/'edge.args').write_text(' '.join(dep['command']+dep['args']) + '\n')
sts = next(x for x in up if x['kind']=='StatefulSet')['spec']['template']['spec']['containers'][0]
(out/'postfix.image').write_text(sts['image'])
ddep = next(x for x in dv if x['kind']=='Deployment')['spec']['template']['spec']['containers'][0]
(out/'dovecot.image').write_text(ddep['image']); (out/'dovecot.args').write_text(' '.join(ddep['command']+ddep['args']) + '\n')
