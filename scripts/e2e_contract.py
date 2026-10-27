#!/usr/bin/env python3
"""HTTP contract E2E: real process, fresh SQLite directory per case, no DB access."""
import argparse
import concurrent.futures
import json
import pathlib
import uuid

from e2e_support import Server, Report, build_binary

ROOT = pathlib.Path(__file__).resolve().parents[1]
TEXT = '徒步旅行自然摄影'


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def expect(s, method, path, status, **kwargs):
    actual, body = s.request(method, path, **kwargs)
    check(actual == status, f'{method} {path}: expected {status}, actual {actual}: {body}')
    return body


def create(s, tenant='demo', **fields):
    body = {'text': TEXT, 'max_candidates': 1, **fields}
    return expect(s, 'POST', '/api/posts', 202, tenant=tenant, key=uuid.uuid4().hex, body=body)


def terminal(s, obj, tenant='demo'):
    return s.wait(obj['job_id'], tenant=tenant, timeout=30)


def upload(s, data=None, caption='徒步旅行照片', name='fixture.png', declared='image/png', tenant='demo'):
    if data is None:
        data = (ROOT / 'fixtures/running.png').read_bytes()
    boundary = 'huanjing-' + uuid.uuid4().hex
    raw = (f'--{boundary}\r\nContent-Disposition: form-data; name="caption"\r\n\r\n{caption}\r\n'
           f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{name}"\r\n'
           f'Content-Type: {declared}\r\n\r\n').encode() + data + f'\r\n--{boundary}--\r\n'.encode()
    return s.request('POST', '/api/uploads', tenant=tenant, raw=raw,
                     headers={'Content-Type': f'multipart/form-data; boundary={boundary}'})


def case_health(s):
    body = expect(s, 'GET', '/api/health', 200)
    check(body['mode'] == 'TEST_FIXTURE', body)
    return body


def invalid_post(body, key='input-test', expected=400, raw=None):
    def run(s):
        value = expect(s, 'POST', '/api/posts', expected, body=body, raw=raw, key=key)
        stats = expect(s, 'GET', '/api/stats', 200)
        check(stats['posts'] == stats['jobs'] == 0, 'invalid request created durable records')
        return {'response': value, 'posts': stats['posts'], 'jobs': stats['jobs']}
    return run


def case_sequential_idempotency(s):
    data = {'text': TEXT, 'max_candidates': 1}
    a = expect(s, 'POST', '/api/posts', 202, body=data, key='same')
    b = expect(s, 'POST', '/api/posts', 202, body=data, key='same')
    check(a['job_id'] == b['job_id'] and a['post_id'] == b['post_id'], [a, b])
    expect(s, 'POST', '/api/posts', 409, body={'text': 'different'}, key='same')
    stats = expect(s, 'GET', '/api/stats', 200)
    check(stats['jobs'] == stats['posts'] == 1, stats)
    return {'job_id': a['job_id'], 'durable_jobs': stats['jobs']}


def case_concurrent_idempotency(s):
    def call(_):
        return s.request('POST', '/api/posts', body={'text': TEXT, 'max_candidates': 1}, key='race')
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
        replies = list(pool.map(call, range(12)))
    check(all(code == 202 for code, _ in replies), replies)
    check(len({b['job_id'] for _, b in replies}) == 1, replies)
    check(len({b['post_id'] for _, b in replies}) == 1, replies)
    job = terminal(s, replies[0][1])
    check(job['status'] == 'COMPLETED', job)
    post = expect(s, 'GET', '/api/posts/' + replies[0][1]['post_id'], 200)
    identities = [(x['avatar_id'], x['kind']) for x in post['interactions']]
    check(len(identities) == len(set(identities)), identities)
    stats = expect(s, 'GET', '/api/stats', 200)
    check(stats['jobs'] == 1, stats)
    return {'requests': len(replies), 'jobs': stats['jobs'], 'unique_actions': len(identities)}


def case_concurrent_conflict(s):
    def call(i):
        return s.request('POST', '/api/posts', body={'text': TEXT + str(i % 2), 'max_candidates': 1}, key='conflict')
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
        replies = list(pool.map(call, range(12)))
    codes = [c for c, _ in replies]
    check(codes.count(202) == 6 and codes.count(409) == 6, replies)
    check(len({b['job_id'] for c, b in replies if c == 202}) == 1, replies)
    return {'accepted': codes.count(202), 'conflicts': codes.count(409)}


def case_tenant_key_namespace(s):
    a = expect(s, 'POST', '/api/posts', 202, body={'text': TEXT}, key='same', tenant='demo')
    b = expect(s, 'POST', '/api/posts', 202, body={'text': TEXT}, key='same', tenant='other')
    check(a['post_id'] != b['post_id'] and a['job_id'] != b['job_id'], [a, b])
    return {'distinct_posts': True, 'distinct_jobs': True}


def owner_resource(s):
    return create(s, fault='slow')


def cross_endpoint(method, resource, body=None, expected=404):
    def run(s):
        obj = owner_resource(s)
        path = resource.format(**obj)
        result = expect(s, method, path, expected, body=body, tenant='other')
        owner = expect(s, 'GET', '/api/posts/' + obj['post_id'], 200)
        check(owner['text'] == TEXT and not owner['deleted'], owner)
        return {'foreign_status': expected, 'owner_unchanged': True, 'response': result}
    return run


def case_cross_lists_stats(s):
    obj = create(s)
    terminal(s, obj)
    posts = expect(s, 'GET', '/api/posts', 200, tenant='other')['posts']
    jobs = expect(s, 'GET', '/api/jobs', 200, tenant='other')['jobs']
    stats = expect(s, 'GET', '/api/stats', 200, tenant='other')
    check(posts == [] and jobs == [], [posts, jobs])
    check(stats['posts'] == stats['jobs'] == stats['interactions'] == 0, stats)
    return {'foreign_posts': len(posts), 'foreign_jobs': len(jobs), 'foreign_interactions': stats['interactions']}


def case_cross_avatar_patch(s):
    expect(s, 'PATCH', '/api/avatars/travel', 200, tenant='other', body={'active': False})
    own = expect(s, 'GET', '/api/avatars', 200)['avatars']
    foreign = expect(s, 'GET', '/api/avatars', 200, tenant='other')['avatars']
    check(next(v for v in own if v['id'] == 'travel')['active'], own)
    check(not next(v for v in foreign if v['id'] == 'travel')['active'], foreign)
    return {'owner_active': True, 'other_active': False}


def case_cross_memory(s):
    expect(s, 'DELETE', '/api/memories/demo-travel1', 404, tenant='other')
    rows = expect(s, 'GET', '/api/memories', 200, tenant='other')['memories']
    check(all(row['id'].startswith('other-') for row in rows), rows)
    own = expect(s, 'GET', '/api/memories', 200)['memories']
    check(not next(row for row in own if row['id'] == 'demo-travel1')['revoked'], own)
    return {'foreign_memory_count': len(rows), 'owner_memory_preserved': True}


def case_cross_budget(s):
    before = expect(s, 'GET', '/api/stats', 200)['budget']
    expect(s, 'POST', '/api/budget', 200, body={'limit': 7}, tenant='other')
    after = expect(s, 'GET', '/api/stats', 200)['budget']
    foreign = expect(s, 'GET', '/api/stats', 200, tenant='other')['budget']
    check(before == after and foreign['limit'] == 7, [before, after, foreign])
    return {'owner_budget': after, 'other_budget': foreign}


def case_upload_magic(s):
    code, body = upload(s, data=b'This is plain text, not a PNG', declared='image/png')
    check(code == 415, [code, body])
    return {'status': code, 'response': body}


def case_upload_detected_type(s):
    code, obj = upload(s, name='misleading.txt', declared='text/plain')
    check(code == 201 and obj['mime'] == 'image/png', [code, obj])
    return {'status': code, 'detected_mime': obj['mime']}


def case_upload_limit(s):
    data = (ROOT / 'fixtures/running.png').read_bytes() + b'\0' * (20 * 1024 * 1024 + 1)
    code, body = upload(s, data=data)
    check(code in (400, 413), [code, body])
    stats = expect(s, 'GET', '/api/stats', 200)
    check(stats['posts'] == 0, stats)
    return {'status': code, 'bytes_sent': len(data), 'response': body}


def case_cross_upload(s):
    code, obj = upload(s)
    check(code == 201, [code, obj])
    expect(s, 'GET', '/api/uploads/' + obj['id'], 404, tenant='other')
    expect(s, 'POST', '/api/posts', 400, tenant='other', key='cross-media',
           body={'text': TEXT, 'attachment_ids': [obj['id']]})
    owner_status, _ = s.request('GET', '/api/uploads/' + obj['id'])
    check(owner_status == 200, owner_status)
    return {'owner_get': owner_status, 'foreign_get': 404, 'foreign_attach': 400}


def case_missing_caption(s):
    code, obj = upload(s, caption='')
    check(code == 201, obj)
    post = create(s, text='', attachment_ids=[obj['id']])
    job = terminal(s, post)
    check(job['status'] == 'REJECTED', job)
    result = expect(s, 'GET', '/api/posts/' + post['post_id'], 200)
    check(result['interactions'] == [], result)
    return {'job_status': job['status'], 'interactions': 0}


def case_caption_media(s):
    code, obj = upload(s)
    check(code == 201, obj)
    post = create(s, text='', attachment_ids=[obj['id']])
    job = terminal(s, post)
    check(job['status'] == 'COMPLETED', job)
    result = expect(s, 'GET', '/api/posts/' + post['post_id'], 200)
    check(len(result['interactions']) >= 1, result)
    return {'job_status': job['status'], 'interactions': len(result['interactions']), 'perception': 'caption_mock'}


def case_private_output(s):
    post = create(s)
    terminal(s, post)
    paths = ['/api/avatars', '/api/memories', '/api/posts', '/api/posts/' + post['post_id'],
             '/api/jobs', '/api/jobs/' + post['job_id'], '/api/stats', '/metrics']
    for path in paths:
        body = expect(s, 'GET', path, 200)
        check('PRIVATE_CANARY' not in json.dumps(body, ensure_ascii=False), f'private canary exposed by {path}')
    return {'checked_endpoints': paths, 'canary_exposure': False}


def case_balanced_accounting(s):
    posts = [create(s) for _ in range(4)]
    jobs = [terminal(s, obj) for obj in posts]
    check(all(j['status'] == 'COMPLETED' for j in jobs), [j['status'] for j in jobs])
    s.until(lambda: expect(s, 'GET', '/api/stats', 200)['outbox_pending'] == 0, timeout=15)
    stats = expect(s, 'GET', '/api/stats', 200)
    costs = sum(op['cost'] for j in jobs for op in j['operations'] if op['status'] == 'SUCCEEDED')
    interactions = sum(len(expect(s, 'GET', '/api/posts/' + obj['post_id'], 200)['interactions']) for obj in posts)
    check(stats['jobs'] == stats['posts'] == 4 and stats['queue_depth'] == 0, stats)
    check(stats['budget']['reserved'] == 0 and stats['budget']['spent'] == costs, [stats, costs])
    check(stats['interactions'] == interactions and stats['outbox_done'] == interactions, [stats, interactions])
    check(sum(stats['statuses'].values()) == stats['jobs'], stats)
    return {'posts': 4, 'spent': costs, 'reserved': 0, 'interactions': interactions, 'outbox_pending': 0}


def case_zero_budget(s):
    expect(s, 'POST', '/api/budget', 200, body={'limit': 0})
    obj = create(s)
    job = terminal(s, obj)
    stats = expect(s, 'GET', '/api/stats', 200)
    check(job['status'] == 'FAILED', job)
    check(stats['budget']['spent'] == stats['budget']['reserved'] == stats['interactions'] == stats['queue_depth'] == 0, stats)
    return {'job_status': job['status'], 'budget': stats['budget'], 'interactions': stats['interactions']}


def case_budget_floor(s):
    obj = create(s)
    terminal(s, obj)
    before = expect(s, 'GET', '/api/stats', 200)['budget']
    check(before['spent'] > 0, before)
    expect(s, 'POST', '/api/budget', 409, body={'limit': before['spent'] - 1})
    after = expect(s, 'GET', '/api/stats', 200)['budget']
    check(before == after, [before, after])
    return {'budget_preserved': after}


def case_edit_whitespace(s):
    obj = create(s)
    expect(s, 'PATCH', '/api/posts/' + obj['post_id'], 400,
           body={'expected_version': 1, 'text': ' \t\n ', 'attachment_ids': []})
    own = expect(s, 'GET', '/api/posts/' + obj['post_id'], 200)
    check(own['text'] == TEXT and own['version'] == 1, own)
    return {'status': 400, 'original_version': own['version']}


def case_deleted_post_job_privacy(s):
    marker = 'DELETED_CONTENT_' + uuid.uuid4().hex
    obj = create(s, text=TEXT + marker)
    terminal(s, obj)
    expect(s, 'DELETE', '/api/posts/' + obj['post_id'], 200)
    expect(s, 'GET', '/api/posts/' + obj['post_id'], 410)
    posts = expect(s, 'GET', '/api/posts', 200)['posts']
    check(all(row['id'] != obj['post_id'] for row in posts), posts)
    job = expect(s, 'GET', '/api/jobs/' + obj['job_id'], 200)
    check(marker not in json.dumps(job), job)
    check(not job['stages'] and not job['candidates'] and not job['operations'], job)
    return {'post_status': 410, 'list_hidden': True, 'job_content_redacted': True}


def case_deleted_attachment_privacy(s):
    code, media = upload(s)
    check(code == 201, media)
    obj = create(s, attachment_ids=[media['id']])
    terminal(s, obj)
    expect(s, 'DELETE', '/api/posts/' + obj['post_id'], 200)
    code, response = s.request('GET', '/api/uploads/' + media['id'])
    check(code in (404, 410), f'deleted sole-parent attachment remained readable: HTTP {code}')
    return {'upload_get_status': code, 'response': response}


def case_shared_attachment_lifecycle(s):
    code, media = upload(s)
    check(code == 201, media)
    first = create(s, attachment_ids=[media['id']])
    second = create(s, attachment_ids=[media['id']])
    terminal(s, first)
    terminal(s, second)
    expect(s, 'DELETE', '/api/posts/' + first['post_id'], 200)
    code, _ = s.request('GET', '/api/uploads/' + media['id'])
    check(code == 200, f'shared attachment hidden while a parent remains live: {code}')
    expect(s, 'DELETE', '/api/posts/' + second['post_id'], 200)
    code, _ = s.request('GET', '/api/uploads/' + media['id'])
    check(code in (404, 410), f'attachment readable after all parents deleted: {code}')
    return {'one_live_parent': 200, 'all_parents_deleted': code}


def case_edited_attachment_binding_history(s):
    code, media = upload(s)
    check(code == 201, media)
    obj = create(s, attachment_ids=[media['id']])
    terminal(s, obj)
    expect(s, 'PATCH', '/api/posts/' + obj['post_id'], 202,
           body={'expected_version': 1, 'text': TEXT, 'attachment_ids': []})
    code, _ = s.request('GET', '/api/uploads/' + media['id'])
    check(code in (404, 410), f'formerly bound attachment became unbound/public after edit: {code}')
    s.restart()
    after_restart, _ = s.request('GET', '/api/uploads/' + media['id'])
    check(after_restart in (404, 410), f'attachment binding history lost after restart: {after_restart}')
    expect(s, 'DELETE', '/api/posts/' + obj['post_id'], 200)
    after_delete, _ = s.request('GET', '/api/uploads/' + media['id'])
    check(after_delete in (404, 410), f'edited attachment became readable after deletion: {after_delete}')
    return {'after_edit': code, 'after_restart': after_restart, 'after_delete': after_delete}


def match_set(s, text):
    obj = create(s, text=text, max_candidates=5)
    job = terminal(s, obj)
    check(job['status'] == 'COMPLETED', job)
    return {candidate['avatar_id'] for candidate in job['candidates']}, job


def case_match_single_topic(s):
    actual, job = match_set(s, '马拉松训练计划')
    check(actual == {'fitness'}, {'expected': ['fitness'], 'actual': sorted(actual), 'job': job})
    return {'candidate_ids': sorted(actual)}


def case_match_history_cannot_override_topic(s):
    earlier, _ = match_set(s, '绘画艺术创作')
    check(earlier == {'art'}, sorted(earlier))
    s.until(lambda: expect(s, 'GET', '/api/stats', 200)['outbox_pending'] == 0, timeout=15)
    actual, job = match_set(s, '马拉松训练计划')
    check(actual == {'fitness'}, {'expected': ['fitness'], 'actual': sorted(actual), 'job': job})
    return {'prior_topic_candidates': sorted(earlier), 'current_candidates': sorted(actual)}


def case_match_multiple_topics(s):
    actual, job = match_set(s, '马拉松之后聊聊咖啡烹饪')
    check(actual == {'fitness', 'food'}, {'expected': ['fitness', 'food'], 'actual': sorted(actual), 'job': job})
    return {'candidate_ids': sorted(actual)}


def case_match_unknown_topic(s):
    actual, job = match_set(s, '今天是普通的一天')
    check(not actual, {'expected': [], 'actual': sorted(actual), 'job': job})
    stats = expect(s, 'GET', '/api/stats', 200)
    check(stats['interactions'] == 0, stats)
    return {'candidate_ids': [], 'status': job['status'], 'interactions': 0}


def case_match_english_word_boundaries(s):
    actual, job = match_set(s, 'Today I am going to start a new habit.')
    check(not actual, {'expected': [], 'actual': sorted(actual), 'job': job})
    return {'candidate_ids': [], 'guards': ['going is not Go', 'start is not art']}


CASES = [
    ('health_mock_mode', '200 and TEST_FIXTURE mode', case_health),
    ('missing_idempotency_key', '400; zero posts/jobs', invalid_post({'text': TEXT}, key=None)),
    ('empty_content', '400; zero posts/jobs', invalid_post({'text': '   '})),
    ('unknown_json_field', '400; zero posts/jobs', invalid_post({'text': TEXT, 'invented': True})),
    ('multiple_json_values', '400; zero posts/jobs', invalid_post(None, raw=b'{"text":"a"} {"text":"b"}')),
    ('text_size_limit', '400; zero posts/jobs', invalid_post({'text': 'x' * 16001})),
    ('candidate_lower_limit', '400; zero posts/jobs', invalid_post({'text': TEXT, 'max_candidates': -1})),
    ('candidate_upper_limit', '400; zero posts/jobs', invalid_post({'text': TEXT, 'max_candidates': 6})),
    ('deadline_lower_limit', '400; zero posts/jobs', invalid_post({'text': TEXT, 'deadline_seconds': 1})),
    ('deadline_upper_limit', '400; zero posts/jobs', invalid_post({'text': TEXT, 'deadline_seconds': 601})),
    ('unknown_fault', '400; zero posts/jobs', invalid_post({'text': TEXT, 'fault': 'unrecognized'})),
    ('attachment_count_limit', '400; zero posts/jobs', invalid_post({'text': TEXT, 'attachment_ids': ['x'] * 5})),
    ('unknown_attachment', '400; zero posts/jobs', invalid_post({'text': TEXT, 'attachment_ids': ['u_missing']})),
    ('sequential_idempotency_conflict', 'one durable identity; changed body 409', case_sequential_idempotency),
    ('concurrent_idempotency', '12 callers share one job; unique actions', case_concurrent_idempotency),
    ('concurrent_different_input_same_key', '6 accepted and 6 conflicts; one winning identity', case_concurrent_conflict),
    ('tenant_idempotency_namespace', 'same key in two tenants creates distinct identities', case_tenant_key_namespace),
    ('cross_tenant_post_read', '404; owner preserved', cross_endpoint('GET', '/api/posts/{post_id}')),
    ('cross_tenant_job_read', '404; owner preserved', cross_endpoint('GET', '/api/jobs/{job_id}')),
    ('cross_tenant_post_edit', '404; owner preserved', cross_endpoint('PATCH', '/api/posts/{post_id}', {'expected_version': 1, 'text': 'changed'})),
    ('cross_tenant_post_delete', '404; owner preserved', cross_endpoint('DELETE', '/api/posts/{post_id}')),
    ('cross_tenant_job_retry', '409; owner preserved', cross_endpoint('POST', '/api/jobs/{job_id}/retry', expected=409)),
    ('cross_tenant_job_reconcile', '404; owner preserved', cross_endpoint('POST', '/api/jobs/{job_id}/reconcile')),
    ('cross_tenant_lists_stats', 'no owner posts/jobs/interactions in other tenant', case_cross_lists_stats),
    ('cross_tenant_avatar_patch', 'mutation affects only caller tenant', case_cross_avatar_patch),
    ('cross_tenant_memories', 'foreign delete 404; list scoped; owner preserved', case_cross_memory),
    ('cross_tenant_budget', 'mutation affects only caller tenant budget', case_cross_budget),
    ('upload_magic_rejects_mislabel', 'text labeled PNG rejected 415', case_upload_magic),
    ('upload_magic_overrides_extension', 'real PNG named txt accepted as image/png', case_upload_detected_type),
    ('upload_size_limit', 'over 20 MiB rejected 400/413', case_upload_limit),
    ('cross_tenant_upload_read_and_attach', 'foreign read 404/attach 400; owner read 200', case_cross_upload),
    ('media_without_caption', 'job REJECTED and zero interactions', case_missing_caption),
    ('media_with_caption', 'caption-mock job COMPLETED with interactions', case_caption_media),
    ('private_canary_never_in_public_api', 'no private canary in eight public outputs', case_private_output),
    ('queue_budget_outbox_accounting', 'four jobs settled; costs/actions/events reconcile', case_balanced_accounting),
    ('zero_budget_no_spend', 'FAILED; no spend/reservation/interactions or pending queue', case_zero_budget),
    ('budget_cannot_drop_below_spend', '409 and prior budget unchanged', case_budget_floor),
    ('edit_whitespace_is_empty_content', '400 and original version/content unchanged', case_edit_whitespace),
    ('deleted_post_job_privacy', 'post 410, list hidden, job sensitive outputs empty', case_deleted_post_job_privacy),
    ('deleted_attachment_privacy', 'sole-parent deleted attachment inaccessible (404/410)', case_deleted_attachment_privacy),
    ('shared_attachment_lifecycle', 'readable while a parent lives; inaccessible after last parent deletion', case_shared_attachment_lifecycle),
    ('edited_attachment_binding_history', 'edit removal and restart preserve bound-upload access restriction', case_edited_attachment_binding_history),
    ('matching_single_topic', 'marathon post recalls only fitness avatar', case_match_single_topic),
    ('matching_history_cannot_override_topic', 'art history cannot qualify art for marathon post', case_match_history_cannot_override_topic),
    ('matching_multiple_topics', 'marathon and coffee recall fitness and food only', case_match_multiple_topics),
    ('matching_unknown_topic', 'unknown topic yields zero candidates and zero interactions', case_match_unknown_topic),
    ('matching_english_word_boundaries', 'going/start do not match Go/art substrings', case_match_english_word_boundaries),
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', default='reports/e2e-contract.json')
    parser.add_argument('--case', help='run only case names containing this substring')
    args = parser.parse_args()
    binary = build_binary()
    report = Report('http-contract', args.output)
    for name, expected, fn in CASES:
        if args.case and args.case not in name:
            continue
        def run(fn=fn):
            with Server(binary=binary, workers=4, seed_tenants='demo,other') as s:
                return fn(s)
        report.run(name, expected, run)
    return report.finish()


if __name__ == '__main__':
    raise SystemExit(main())
