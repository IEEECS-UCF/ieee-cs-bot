"""Writes what the Python job helpers return for the inputs below, for the Go tests to match.

    python3 internal/jobs/testdata/python_expect.py
"""
import os, sys, types, json

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
sys.path.insert(0, ROOT)
# Stub what job_feed imports at module level
discord = types.ModuleType("discord"); ext = types.ModuleType("discord.ext")
commands = types.ModuleType("discord.ext.commands"); tasks = types.ModuleType("discord.ext.tasks")
commands.Cog = object
class _Loop:
    def __init__(self, f): self.f = f
    def before_loop(self, g): return g
tasks.loop = lambda **kw: (lambda f: _Loop(f))
ext.commands, ext.tasks = commands, tasks
discord.ext = ext
sys.modules.update({"discord": discord, "discord.ext": ext, "discord.ext.commands": commands, "discord.ext.tasks": tasks,
                    "aiohttp": types.ModuleType("aiohttp")})
fmt = types.ModuleType("bot.utils.formatting"); fmt.create_base_embed = None; fmt.IEEE_BLUE = 0
al = types.ModuleType("bot.utils.alerts"); al.alert = None
sys.modules["bot.utils.formatting"] = fmt; sys.modules["bot.utils.alerts"] = al
from bot.utils import job_worker as w
from bot.cogs import job_feed as f

out = {}
locs = ["Austin, TX", "US-FL-Largo", "US, CA, Santa Clara", "Toronto, ON", "CA-ON-Ottawa", "Remote", "London, UK",
        "Kraków", "KRAKÓW", "São Paulo", "SÃO PAULO", "Zürich", "Montréal, QC", "texas", "us remote", "Bengaluru",
        "Parisé", "éLondon", "Austin, TX", "austin, tx", "USA", "U.S.", "3 Locations", "KNV I", "Czechia",
        "Brno, Czeché", "Ontario", "Mexico City", "New Mexico", "Cambridge, UK", "Cambridge, MA", "Paris, TX",
        "Washington, DC", "Seoul, Korea", "Dublin, CA", "Ⅳ Paris", "Paris²", "Paris_x", "Korea", "ⅰndia"]
out["classify"] = {l: w.classify_location(l) for l in locs}
titles = ["Software Engineer Intern - Summer 2027", "2026 Fall Co-op / Spring 2027", "Summer/Fall Intern",
          "SUMMER 2027 Intern", "Firmware Engineer I", "Firmware Engineer I", "Senior GPU Engineer",
          "Associate Firmware Engineer", "CPU Design Verification Intern", "Quantitative Trading Intern, C++",
          "Kernel Engineer, New Grad 2026", "Linux Driver Developer", "Display Driver Engineer Co-op",
          "Rust Développeur", "Développeur embedded", "embeddedé", "Ingénieur Firmware Stagiaire",
          "Compiler Engineering Internship (Fall 2026)", "GPU Compute Intern – Winter 2027", "Student Researcher, HPC",
          "Engineer 1, Embedded Software", "Rotational Development Program - Hardware", "Technician Intern, Firmware",
          "Machine Learning Intern", "Sr. Embedded Engineer", "ſtudent embedded", "Ⅰntern embedded"]
out["terms"] = {t: w.terms_from_title(t) for t in titles}
out["topics"] = {t: f.match_topics(f.normalize(t)) for t in titles}
out["track"] = {t: f.infer_track(f.normalize(t)) for t in titles}
student = w.student_filter({'include': f"{f.INTERN}|{f.NEW_GRAD}", 'exclude': f.SENIOR})
out["student"] = {t: student(t) for t in titles}
keep = w.title_filter({'include': list(f.TOPICS.values()), 'exclude': f.EXCLUDE})
out["keep"] = {t: keep(t, "") for t in titles}
posted = ["Posted Today", "Posted Yesterday", "Posted 30+ Days Ago", "posted 5 days ago", "Posted 1 Day Ago", "", "New"]
out["days"] = {p: w.workday_days_ago({"postedOn": p}) for p in posted}
companies = [("Astera Labs Inc.", "Software Engineer Intern - Summer 2027"), ("The Boeing Company", "Embedded Software Engineering Internship"),
             ("Cboe Global Markets", "Engineer II, C++/C# Developer"), ("İstanbul Tech", "Firmware Interns 2026"),
             ("NVIDIA", "Systems Software Engineer, New College Grad 2026")]
out["dedupe"] = [f.dedupe_key(c, t) for c, t in companies]
pairs = [("cboe|1 engineer", "cboe global markets|1 engineer"), ("cboe global|1 engineer", "cboe|1 engineer"),
         ("|intern", "acme|intern"), ("acme|intern", "acmex|intern"), ("a b|x", "a c|x"), ("a|x", "a|y")]
out["same"] = [f.same_role(a, b) for a, b in pairs]
urls = ["https://boards.greenhouse.io/spacex/jobs/123?gh_src=abc&t=1&utm_source=x",
        " https://Jobs.Lever.co/zoox/abc-123/apply?lever-source=LinkedIn ",
        "https://nvidia.wd5.myworkdayjobs.com/en-US/NVIDIAExternalCareerSite/job/US-CA-Santa-Clara/GPU-Intern_JR1/apply/",
        "https://nvidia.wd5.myworkdayjobs.com/NVIDIAExternalCareerSite/job/X_JR2",
        "https://intel.wd1.myworkdayjobs.com/en-us",
        "https://example.com/careers/job;jsessionid=XYZ?id=5&ref=simplify&Source=a&q=a+b%20c&empty=&flag",
        "https://example.com/a/application", "https://example.com/apply", "https://example.com",
        "https://example.com/x?q=%zz&r=%E2%82%AC&s=%FF", "https://example.com/x#frag", "example.com/path",
        "https://EXAMPLE.com/Path/To/Job/", "https://x.com/a;p/b;q?z=1"]
out["urls"] = {u: f.url_key(u) for u in urls}
out["locations"] = [f.format_locations(l) for l in ([], ["A"], ["A", "B", "C"], ["A", "B", "C", "D", "E"])]
def listing(**kw):
    d = {'track': None, 'logo_fallback': None, 'terms': [], 'sponsorship': None, 'degrees': [], 'countries': None,
         'active': True, 'company_url': None, 'source': 'Simplify'}
    d.update(kw); return d
L = [
    listing(id="a1", title="Firmware Engineer Intern", company="Acme Inc.", url="https://acme.com/jobs/1?utm_source=simplify",
            locations=["Orlando, FL"], terms=["Summer 2027"], date_posted=200, track="Internship", company_url="https://simplify.jobs/c/Acme",
            sponsorship="Offers Sponsorship"),
    listing(id="a2", title="Firmware Engineer Intern", company="Acme", url="https://acme.com/jobs/2",
            locations=["Austin, TX", "Orlando, FL"], terms=["Fall 2027"], date_posted=150, source="vanshb03", logo_fallback="https://logo/a"),
    listing(id="a3", title="Firmware Engineering Internship", company="Acme Robotics", url="https://acme.com/jobs/3/apply",
            locations=["Remote"], terms=[], date_posted=300, source="Acme Robotics"),
    listing(id="b1", title="Senior Firmware Engineer", company="Beta", url="https://beta.com/1", locations=["Austin, TX"], date_posted=200),
    listing(id="c1", title="GPU Software Intern", company="Gamma", url="https://gamma.com/1", locations=["London, UK"], date_posted=200),
    listing(id="c2", title="GPU Software Intern", company="Gamma", url="https://gamma.com/2", locations=["London, UK", "Remote"], date_posted=200),
    listing(id="d1", title="Kernel Engineer", company="Delta", url="https://delta.com/1", locations=["Toronto, ON"], date_posted=200),
    listing(id="d2", title="Kernel Engineer, New Grad", company="Delta", url="https://delta.com/1?ref=x", locations=["Ottawa, ON"], date_posted=100),
    listing(id="e1", title="Embedded Intern", company="Eps", url="https://eps.com/1", locations=["Somewhere"], countries=["Germany"], date_posted=200),
    listing(id="e2", title="Embedded Intern", company="Eps", url="https://eps.com/2", locations=["KNV I, USA"], countries=["United States of America"], date_posted=50),
    listing(id="f1", title="Rust Intern", company="Phi", url="https://phi.com/1", locations=[], date_posted=200, active=False),
    listing(id="g1", title="Machine Learning Intern", company="Gee", url="https://gee.com/1", locations=[], date_posted=200),
    listing(id="h1", title="Cboe  C++   Intern", company="Cboe", url="https://cboe.com/1", locations=[], date_posted=200, degrees=["Master's", "PhD"]),
    listing(id="h2", title="Cboe C++ Intern", company="Cboe Global Markets", url="https://cboe.com/2", locations=["Chicago, IL"], date_posted=210),
]
out["select_input"] = L
roles = f.select_jobs(L, 100)
out["select"] = [{k: r[k] for k in ('keys', 'ids', 'urls', 'track', 'topics', 'title', 'company', 'locations', 'terms', 'logo_fallback', 'date_posted', 'source')} for r in roles]
out["highlights"] = [f.build_highlights(r) for r in roles]
out["season_split_input"] = [{"id": "x", "active": True, "date_posted": 5, "season": "Summer / Fall", "url": "u", "title": "T", "company_name": "C"},
    {"id": 7, "active": 1, "date_posted": 5, "terms": ["N/A", "Spring 2027"], "url": "u2", "is_visible": False},
    {"id": 8, "active": True, "date_posted": 5, "terms": [], "season": "Summer", "url": "u3", "locations": ["A"]},
    {"id": 9, "active": True, "date_posted": 1, "url": "u4"}]
out["season_split"] = w.parse_github_list(out["season_split_input"], "S", "Internship", 2)
for l in out["season_split"]:
    l["id"] = str(l["id"])
gh = {"meta": {"total": 2}, "jobs": [
    {"id": 4012345006, "title": "Firmware Intern", "absolute_url": "https://g/1", "location": {"name": "Hawthorne, CA; Remote | Austin, TX"}, "first_published": "2026-10-20T10:00:00-04:00", "updated_at": "2026-10-21T10:00:00-04:00"},
    {"id": 2, "title": "Old", "absolute_url": "https://g/2", "location": None, "updated_at": "2020-01-01T00:00:00Z"}]}
lever = [{"id": "abc", "text": "Embedded Intern Summer 2027", "hostedUrl": "https://l/1", "categories": {"location": "Foster City, CA", "allLocations": []}, "workplaceType": "remote", "createdAt": 1792000000123},
         {"id": "def", "text": "x", "hostedUrl": "https://l/2", "categories": {"allLocations": ["Remote", "Seattle, WA"]}, "workplaceType": "remote", "createdAt": 1792000000999}]
ashby = {"apiVersion": "1", "jobs": [
    {"id": "u1", "title": "Compiler Intern", "jobUrl": "https://a/1", "location": "Sunnyvale, CA", "secondaryLocations": [{"location": "Toronto, ON"}, {"location": None}], "isRemote": True, "publishedAt": "2026-10-20T15:53:41.397+00:00"},
    {"id": "u2", "title": "Hidden", "jobUrl": "https://a/2", "isListed": False, "publishedAt": "2026-10-20T15:53:41.397+00:00"}]}
out["boards_input"] = {"greenhouse": gh, "lever": lever, "ashby": ashby}
out["boards"] = {k: w.parse_board(d, k, "slug", "Co Name", "co.com", 1700000000) for k, d in (("greenhouse", gh), ("lever", lever), ("ashby", ashby))}
facets = [{"facetParameter": "workerSubType", "values": [{"id": "s1", "descriptor": "Intern (Fixed Term)"}, {"id": "s2", "descriptor": "Regular"}, {"descriptor": "College Grad"}]},
          {"facetParameter": "jobFamilyGroup", "values": [{"id": "j1", "descriptor": "Engineering"}, {"id": "j2", "descriptor": "Sales"}]}]
out["facets_input"] = facets
out["facets"] = [w.facet_ids(facets, "workerSubType", w.WORKDAY_STUDENT_TYPES), w.facet_ids(facets, "jobFamilyGroup", w.WORKDAY_ENGINEERING_FAMILIES), w.facet_ids(facets, "x", w.WORKDAY_STUDENT_TYPES)]
JOBS = ("classify", "terms", "days", "season_split_input", "season_split", "boards_input", "boards", "facets_input", "facets")
for path, keys in ((os.path.join(ROOT, "internal", "jobs", "testdata", "python.json"), JOBS),
                   (os.path.join(ROOT, "internal", "cogs", "jobfeed", "testdata", "python.json"), [k for k in out if k not in JOBS])):
    with open(path, "w") as fp:
        json.dump({k: out[k] for k in keys}, fp, ensure_ascii=False, indent=1)
        fp.write("\n")
