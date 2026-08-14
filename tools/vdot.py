import math

def vo2_of_v(v):  # v in m/min
    return -4.60 + 0.182258*v + 0.000104*v*v

def v_of_vo2(vo2):
    a=0.000104; b=0.182258; c=-4.60-vo2
    return (-b + math.sqrt(b*b-4*a*c))/(2*a)

def pct_max(t):  # t in minutes
    return 0.8 + 0.1894393*math.exp(-0.012778*t) + 0.2989558*math.exp(-0.1932605*t)

def vdot(dist_m, t_sec):
    t=t_sec/60.0
    v=dist_m/t
    return vo2_of_v(v)/pct_max(t)

def time_for(dist_m, VDOT):
    # solve for t
    lo,hi=0.2, 400.0
    for _ in range(200):
        mid=(lo+hi)/2
        v=dist_m/mid
        vd=vo2_of_v(v)/pct_max(mid)
        if vd>VDOT: lo=mid
        else: hi=mid
    return (lo+hi)/2*60

def fmt(s):
    s=round(s)
    h=s//3600; m=(s%3600)//60; sec=s%60
    return (f"{h}:{m:02d}:{sec:02d}" if h else f"{m}:{sec:02d}")

def pace_mi(v):  # v m/min -> min/mile string
    p=1609.344/v
    return fmt(p*60)

def pace_km(v):
    p=1000.0/v
    return fmt(p*60)

def ms(t): 
    parts=[int(x) for x in t.split(':')]
    s=0
    for p in parts: s=s*60+p
    return s

efforts=[
 ("2026-04-11","400m",400,ms("1:44")),
 ("2026-04-11","800m",800,ms("3:32")),
 ("2026-04-11","1K",1000,ms("4:25")),
 ("2026-04-11","1 mile",1609.344,ms("7:11")),
 ("2026-04-11","2 mile",3218.688,ms("14:44")),
 ("2026-04-11","5K",5000,ms("23:11")),
 ("2026-04-11","10K",10000,ms("49:23")),
 ("2026-07-25","1 mile",1609.344,ms("7:38")),
 ("2026-07-25","5K",5000,ms("24:58")),
 ("2026-07-25","10K",10000,ms("55:59")),
 ("2026-07-20","5K",5000,ms("27:38")),
 ("2026-07-20","10K",10000,ms("60:04")),
 ("2026-07-20","15K",15000,ms("1:34:03")),
 ("2026-05-02","half",21097.5,ms("1:46:21")),
 ("2026-07-23","half",21097.5,ms("1:49:23")),
]
print("=== RAW VDOT of each effort (as run) ===")
for d,n,dist,t in efforts:
    v=dist/(t/60.0)
    print(f"{d} {n:8s} {fmt(t):>8s}  pace {pace_mi(v)}/mi  {pace_km(v)}/km   VDOT {vdot(dist,t):.1f}")

print()
print("=== HEAT CROSS-CHECK: what heat factor reconciles Jul25 with Apr11 ===")
for name,apr,jul in [("5K",ms("23:11"),ms("24:58")),("1 mile",ms("7:11"),ms("7:38")),("10K",ms("49:23"),ms("55:59"))]:
    print(f"{name}: Apr {fmt(apr)} vs Jul {fmt(jul)} -> Jul is {100*(jul/apr-1):.1f}% slower")
for f in [0.93,0.94,0.95]:
    print(f"  Jul25 5K 24:58 corrected by {100*(1-f):.0f}% -> {fmt(ms('24:58')*f)} (VDOT {vdot(5000,ms('24:58')*f):.1f})")
    print(f"  Jul25 1mi 7:38 corrected by {100*(1-f):.0f}% -> {fmt(ms('7:38')*f)} (VDOT {vdot(1609.344,ms('7:38')*f):.1f})")
print(f"  Jul23 half 1:49:23 corrected by 5% -> {fmt(ms('1:49:23')*0.95)} (VDOT {vdot(21097.5,ms('1:49:23')*0.95):.1f})")
print(f"  Jul23 half 1:49:23 corrected by 7% -> {fmt(ms('1:49:23')*0.93)} (VDOT {vdot(21097.5,ms('1:49:23')*0.93):.1f})")

print()
print("=== RACE ADJUSTMENT on Apr11 5K 23:11 ===")
for pct in [0,2,3,4,5]:
    t=ms("23:11")*(1-pct/100)
    print(f"  -{pct}% -> 5K {fmt(t)}  VDOT {vdot(5000,t):.1f}")
print("=== RACE ADJUSTMENT on May2 half 1:46:21 ===")
for pct in [0,2,3,4]:
    t=ms("1:46:21")*(1-pct/100)
    print(f"  -{pct}% -> half {fmt(t)}  VDOT {vdot(21097.5,t):.1f}")
