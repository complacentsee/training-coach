import math
def fmt(s):
    s=round(s); h=s//3600; m=(s%3600)//60; sec=s%60
    return (f"{h}:{m:02d}:{sec:02d}" if h else f"{m}:{sec:02d}")
def ms(t):
    s=0
    for p in t.split(':'): s=s*60+int(p)
    return s

base={"Recovery 10:20":ms("10:20"),"Easy 9:20":ms("9:20"),"Marathon 8:15":ms("8:15"),
      "Threshold 7:41":ms("7:41"),"10K 7:34":ms("7:34"),"5K 7:18":ms("7:18"),"I 6:58":ms("6:58")}
print("=== HEAT-ADJUSTED PACES (min/mile) ===")
pcts=[0,1,2,3,4.5,6,8,10]
sums=["<100","100-110","110-120","120-130","130-140","140-150","150-160","160-170","170-180"]
print(f"{'pace':18s}"+"".join(f"{str(p)+'%':>9s}" for p in pcts))
for k,v in base.items():
    print(f"{k:18s}"+"".join(f"{fmt(v*(1+p/100)):>9s}" for p in pcts))
print()
print("=== seconds/mile penalty ===")
for k,v in base.items():
    print(f"{k:18s}"+"".join(f"{'+'+str(round(v*p/100)):>9s}" for p in pcts))
print()
print("=== July 25 decoupling ===")
p=[518,508,498,489,468,484,510,514]; h=[144,162,168,176,179,176,171,173]
ef=[(1/pp)/hh*1e6 for pp,hh in zip(p,h)]
for i,(pp,hh,e) in enumerate(zip(p,h,ef),1):
    print(f" mi{i}: {fmt(pp)} @ {hh}  EF={e:.3f}")
f1=sum(ef[:4])/4; f2=sum(ef[4:])/4
print(f" 1st half EF {f1:.3f}  2nd half EF {f2:.3f}  decoupling {100*(f1-f2)/f1:.1f}%")
print(f" mile1 vs mile8 EF drop: {100*(ef[0]-ef[7])/ef[0]:.1f}%  (same pace {fmt(p[0])} vs {fmt(p[7])}, HR {h[0]}->{h[7]} = +{h[7]-h[0]})")
avgp=sum(p)/8; avgh=sum(h)/8
print(f" run avg: {fmt(avgp)}/mi @ HR {avgh:.0f}")
