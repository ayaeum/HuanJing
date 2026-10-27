#!/usr/bin/env python3
"""Planning calculator. Prices are explicit hypothetical inputs, never a provider quote."""
import argparse, json
p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--posts-per-second',type=float,default=10)
p.add_argument('--pass-rate',type=float,default=.9)
p.add_argument('--candidates',type=float,default=4)
p.add_argument('--comment-rate',type=float,default=.4)
p.add_argument('--attempts',type=float,default=1.1)
p.add_argument('--input-price',type=float,required=True,help='Hypothetical currency units per million input tokens')
p.add_argument('--output-price',type=float,required=True,help='Hypothetical currency units per million output tokens')
p.add_argument('--currency',default='USD')
p.add_argument('--output')
a=p.parse_args()
if any(v<0 for v in (a.posts_per_second,a.candidates,a.input_price,a.output_price)) or not 0<=a.pass_rate<=1 or not 0<=a.comment_rate<=1 or a.attempts<1:p.error('nonnegative inputs, probabilities 0..1, attempts >= 1 required')
base=a.posts_per_second*a.pass_rate*a.candidates*a.attempts
stages=[]
for name,r,tin,tout,seconds in [('decision',base,600,200,.8),('comment',base*a.comment_rate,900,300,1.5)]:
 cost=r*(tin*a.input_price+tout*a.output_price)/1e6*86400
 stages.append(dict(stage=name,calls_per_second=r,input_tokens_per_call=tin,output_tokens_per_call=tout,mean_call_seconds=seconds,mean_inflight=r*seconds,daily_cost=round(cost,6)))
r={'scope':'Hypothetical steady 24-hour workload; only decision and comment stages. Excludes moderation, understanding, embeddings, media, infrastructure, taxes and discounts. Not actual spend or provider quote.', 'parameters':vars(a),'stages':stages,'minimum_rpm':sum(x['calls_per_second'] for x in stages)*60,'minimum_tpm':sum(x['calls_per_second']*(x['input_tokens_per_call']+x['output_tokens_per_call']) for x in stages)*60,'daily_cost':round(sum(x['daily_cost'] for x in stages),6),'monthly_30_days_cost':round(sum(x['daily_cost'] for x in stages)*30,6)}
s=json.dumps(r,ensure_ascii=False,indent=2)
if a.output:
 from pathlib import Path
 Path(a.output).write_text(s+'\n')
print(s)
