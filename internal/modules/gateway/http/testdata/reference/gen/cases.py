# Writes ../cases.json: the requests the differential test of /mcp replays (see README.md).
import json, os
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "cases.json")
CT={"content-type":"application/json","accept":"application/json, text/event-stream"}
M={"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}
P={"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"1"}}
cases=[]
def add(name, body, hdr=None, method="POST", drop=()):
    h=dict(CT)
    for k in drop: h.pop(k,None)
    if hdr: h.update(hdr)
    b=body if isinstance(body,str) else json.dumps(body,separators=(',',':'),ensure_ascii=False)
    cases.append({"name":name,"method":method,"headers":h,"body":b})
def req(i,m,params=None,**extra):
    d={"jsonrpc":"2.0","id":i,"method":m}
    if params is not None: d["params"]=params
    d.update(extra); return d
def notif(m,params=None):
    d={"jsonrpc":"2.0","method":m}
    if params is not None: d["params"]=params
    return d
mod={"mcp-protocol-version":"2026-07-28"}
def mh(method, name=None):
    h={"mcp-protocol-version":"2026-07-28","mcp-method":method}
    if name: h["mcp-name"]=name
    return h
def env(**p):
    d={"_meta":dict(M)}; d.update(p); return d
# initialize
for v in ["2025-03-26","2025-06-18","2025-11-25","2024-11-05","1999-01-01","2026-07-28",""]:
    add("init "+v, req(1,"initialize",dict(P,protocolVersion=v)))
add("init bare", req(1,"initialize"))
add("init params {}", req(1,"initialize",{}))
add("init only protocolVersion", req(1,"initialize",{"protocolVersion":"2025-11-25"}))
add("init no clientInfo", req(1,"initialize",{"protocolVersion":"2025-11-25","capabilities":{}}))
add("init clientInfo no version", req(1,"initialize",dict(P,clientInfo={"name":"x"})))
add("init capabilities array", req(1,"initialize",dict(P,capabilities=[])))
add("init protocolVersion 5", req(1,"initialize",dict(P,protocolVersion=5)))
add("init valid + header 1999", req(1,"initialize",P), {"mcp-protocol-version":"1999-01-01"})
add("init bare + header 1999", req(1,"initialize"), {"mcp-protocol-version":"1999-01-01"})
add("init valid + header 2025-06-18", req(1,"initialize",P), {"mcp-protocol-version":"2025-06-18"})
add("init valid + modern header", req(1,"initialize",P), mod)
add("init bare + modern header", req(1,"initialize"), mod)
add("init with classic claim", req(1,"initialize",dict(P,_meta={"io.modelcontextprotocol/protocolVersion":"2025-06-18"})))
add("init with modern claim", req(1,"initialize",dict(P,_meta=M)), mh("initialize"))
add("init with string id", req("a","initialize",P))
# classic requests
add("tools/list", req(2,"tools/list"))
add("ping", req(2,"ping"))
add("ping with params", req(2,"ping",{}))
add("list_servers", req(3,"tools/call",{"name":"list_servers","arguments":{}}))
add("list_servers no args", req(3,"tools/call",{"name":"list_servers"}))
add("unknown tool", req(3,"tools/call",{"name":"nope","arguments":{}}))
add("tools/call no name", req(3,"tools/call",{"arguments":{}}))
add("tools/call no params", req(3,"tools/call"))
add("tools/call args array", req(3,"tools/call",{"name":"list_servers","arguments":[1]}))
add("tools/call args null", req(3,"tools/call",{"name":"list_servers","arguments":None}))
add("tools/call name 5", req(3,"tools/call",{"name":5}))
add("search limit 26", req(3,"tools/call",{"name":"search_tools","arguments":{"query":"x","limit":26}}))
add("search two issues", req(3,"tools/call",{"name":"search_tools","arguments":{"query":"","limit":0}}))
add("search ok", req(3,"tools/call",{"name":"search_tools","arguments":{"query":"x"}}))
add("describe x.y", req(3,"tools/call",{"name":"describe_tool","arguments":{"tool":"x.y"}}))
add("describe missing", req(3,"tools/call",{"name":"describe_tool","arguments":{}}))
add("call_tool ok", req(3,"tools/call",{"name":"call_tool","arguments":{"tool":"a.b"}}))
add("call_tool args no", req(3,"tools/call",{"name":"call_tool","arguments":{"tool":"a.b","arguments":"no"}}))
add("no/such", req(2,"no/such"))
add("resources/list", req(2,"resources/list"))
add("server/discover classic", req(2,"server/discover"))
add("notifications as request", req(2,"notifications/initialized"))
# header variants
for v in ["1999-01-01","2025-06-18","2024-10-07","2026-07-28","2027-01-01","2025-11-25"]:
    add("tools/list header "+v, req(2,"tools/list"), {"mcp-protocol-version":v})
add("tools/list header 2027 empty meta", req(2,"tools/list",{"_meta":{}}), {"mcp-protocol-version":"2027-01-01"})
add("tools/list modern header meta partial", req(2,"tools/list",{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}), {})
# accept / content type
add("accept json only", req(2,"tools/list"), {"accept":"application/json"})
add("accept sse only", req(2,"tools/list"), {"accept":"text/event-stream"})
add("accept none", req(2,"tools/list"), drop=("accept",))
add("accept */*", req(2,"tools/list"), {"accept":"*/*"})
add("ct text/plain", req(2,"tools/list"), {"content-type":"text/plain"})
add("ct none", req(2,"tools/list"), drop=("content-type",))
add("ct charset", req(2,"tools/list"), {"content-type":"application/json; charset=utf-8"})
add("ct and accept bad", req(2,"tools/list"), {"content-type":"text/plain","accept":"text/html"})
add("invalid json bad accept", "{", {"accept":"text/html"})
add("shape bad accept", '{"hello":1}', {"accept":"text/html"})
add("notification bad accept", notif("notifications/initialized"), {"accept":"text/html"})
# bodies
for n,b in [("empty",""),("brace","{"),("number","5"),("null","null"),("string",'"x"'),("hello",'{"hello":1}'),("empty array","[]"),
            ("jsonrpc 1.0",'{"jsonrpc":"1.0","id":1,"method":"tools/list"}'),("no jsonrpc",'{"id":1,"method":"tools/list"}'),
            ("id null",'{"jsonrpc":"2.0","id":null,"method":"tools/list"}'),("id 1.5",'{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}'),
            ("id true",'{"jsonrpc":"2.0","id":true,"method":"tools/list"}'),("id object",'{"jsonrpc":"2.0","id":{},"method":"tools/list"}'),
            ("params array",'{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}'),("params null",'{"jsonrpc":"2.0","id":1,"method":"tools/list","params":null}'),
            ("method 5",'{"jsonrpc":"2.0","id":1,"method":5}'),("extra key",'{"jsonrpc":"2.0","id":7,"method":"tools/list","extra":1}'),
            ("Method key",'{"jsonrpc":"2.0","id":1,"Method":"tools/list"}'),("result response",'{"jsonrpc":"2.0","id":3,"result":{}}'),
            ("error response",'{"jsonrpc":"2.0","id":3,"error":{"code":1,"message":"m"}}'),("error response no id",'{"jsonrpc":"2.0","error":{"code":1,"message":"m"}}'),
            ("result not object",'{"jsonrpc":"2.0","id":3,"result":5}'),("error code float",'{"jsonrpc":"2.0","id":3,"error":{"code":1.5,"message":"m"}}'),
            ("id float string method",'{"jsonrpc":"2.0","id":"a","method":"tools/list","x":1}'),
            ("whitespace body","   \n "),("dup method",'{"jsonrpc":"2.0","id":1,"method":"ping","method":"tools/list"}'),
            ("name variants",'{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_servers","Name":"call_tool"}}'),
            ("Name only",'{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"Name":"list_servers"}}')]:
        add("body "+n, b)
add("result response header 1999", '{"jsonrpc":"2.0","id":3,"result":{}}', {"mcp-protocol-version":"1999-01-01"})
# _meta shapes
for n,meta in [("progressToken string",{"progressToken":"t"}),("progressToken int",{"progressToken":5}),("progressToken float",{"progressToken":1.5}),
               ("progressToken bool",{"progressToken":True}),("progressToken object",{"progressToken":{}}),("related-task ok",{"io.modelcontextprotocol/related-task":{"taskId":"t"}}),
               ("related-task bad",{"io.modelcontextprotocol/related-task":{"taskId":5}}),("meta null",None),("meta array",[])]:
    add("meta "+n, req(2,"tools/list",{"_meta":meta}))
    add("notif meta "+n, notif("notifications/initialized",{"_meta":meta}))
    add("batch meta "+n, [req(2,"tools/list",{"_meta":meta})])
add("notif params array", notif("notifications/initialized",[]))
# notifications
add("notif initialized", notif("notifications/initialized"))
add("notif cancelled", notif("notifications/cancelled",{"requestId":1}))
add("notif header 1999", notif("notifications/initialized"), {"mcp-protocol-version":"1999-01-01"})
add("notif modern header", notif("notifications/initialized"), mod)
add("notif header 2027", notif("notifications/initialized"), {"mcp-protocol-version":"2027-01-01"})
add("notif modern header method mismatch", notif("notifications/initialized"), {"mcp-protocol-version":"2026-07-28","mcp-method":"x"})
add("notif claim", notif("notifications/initialized",{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}))
add("notif claim 5", notif("notifications/initialized",{"_meta":{"io.modelcontextprotocol/protocolVersion":5}}))
add("notif claim header mismatch", notif("notifications/initialized",{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}), {"mcp-protocol-version":"2025-11-25"})
add("notif claim method mismatch", notif("notifications/initialized",{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}), {"mcp-method":"x"})
add("notif claim 2099", notif("notifications/initialized",{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}))
add("notif claim classic", notif("notifications/initialized",{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18"}}))
add("notif jsonrpc 1.0 claim", {"jsonrpc":"1.0","method":"notifications/initialized","params":{"_meta":M}})
# batches
L=req(1,"tools/list"); PING=req(2,"ping"); IN=req(9,"initialize",P); BARE=req(9,"initialize")
for n,b,h in [("[]",[],None),("[list]",[L],None),("[list,ping]",[L,PING],None),("[init]",[IN],None),("[bare init]",[BARE],None),
              ("[bare init, ping]",[BARE,PING],None),("[init, ping]",[IN,PING],None),("[bare init] 1999",[BARE],{"mcp-protocol-version":"1999-01-01"}),
              ("[init] 1999",[IN],{"mcp-protocol-version":"1999-01-01"}),("[list] 1999",[L],{"mcp-protocol-version":"1999-01-01"}),
              ("[list] modern",[L],mod),("[init] modern",[IN],mod),("[bare init] modern",[BARE],mod),
              ("[notif]",[notif("notifications/initialized")],None),("[notif] 1999",[notif("notifications/initialized")],{"mcp-protocol-version":"1999-01-01"}),
              ("[response]",[{"jsonrpc":"2.0","id":3,"result":{}}],None),("[invalid]",[{"x":1}],None),
              ("[jsonrpc 1.0 + claim]",[{"jsonrpc":"1.0","id":1,"method":"tools/list","params":{"_meta":M}}],None),
              ("[claim not string]",[req(1,"tools/list",{"_meta":{"io.modelcontextprotocol/protocolVersion":5}})],None),
              ("[list, invalid]",[L,{"x":1}],None),("[invalid, claim]",[{"x":1},req(1,"tools/list",env())],None),
              ("[claim]",[req(1,"tools/list",env())],None),("[[list]]",[[L]],None),("[5]",[5],None),("[null]",[None],None),
              ("[list, unknown]",[L,req(4,"no/such")],None),("[list] bad accept",[L],{"accept":"text/html"}),
              ("[invalid] bad accept",[{"x":1}],{"accept":"text/html"}),("[init,init]",[IN,dict(IN,id=10)],None),
              ("[dup ids]",[L,dict(L)],None),("[list, notif, response]",[L,notif("n"),{"jsonrpc":"2.0","id":3,"result":{}}],None)]:
    add("batch "+n, b, h)
add("batch 100", [L]*100)
add("batch 101", [L]*101)
add("batch 101 bad accept", [L]*101, {"accept":"text/html"})
add("batch 101, invalid 101st", [L]*100+[{"x":1}])
add("batch 102, invalid 102nd", [L]*101+[{"x":1}])
add("batch 102, claim 102nd", [L]*101+[req(1,"tools/list",env())])
# modern
add("m discover", req(1,"server/discover",env()), mh("server/discover"))
add("m list", req(1,"tools/list",env()), mh("tools/list"))
add("m call list_servers", req(1,"tools/call",env(name="list_servers",arguments={})), mh("tools/call","list_servers"))
add("m call no Mcp-Name", req(1,"tools/call",env(name="list_servers",arguments={})), mh("tools/call"))
add("m call Mcp-Name other", req(1,"tools/call",env(name="list_servers",arguments={})), mh("tools/call","call_tool"))
add("m call base64", req(1,"tools/call",env(name="list_servers",arguments={})), mh("tools/call","=?base64?bGlzdF9zZXJ2ZXJz?="))
add("m call base64 broken", req(1,"tools/call",env(name="list_servers",arguments={})), mh("tools/call","=?base64?b!?="))
add("m call unknown tool", req(1,"tools/call",env(name="nope",arguments={})), mh("tools/call","nope"))
add("m call search bad", req(1,"tools/call",env(name="search_tools",arguments={"query":"x","limit":26})), mh("tools/call","search_tools"))
add("m call no name", req(1,"tools/call",env(arguments={})), mh("tools/call"))
add("m no version header", req(1,"tools/list",env()), {"mcp-method":"tools/list"})
add("m no method header", req(1,"tools/list",env()), mod)
add("m method mismatch", req(1,"tools/list",env()), mh("tools/call"))
add("m version mismatch", req(1,"tools/list",env()), {"mcp-protocol-version":"2025-11-25","mcp-method":"tools/list"})
add("m claim 2099", req(1,"tools/list",{"_meta":dict(M,**{"io.modelcontextprotocol/protocolVersion":"2099-01-01"})}), {"mcp-protocol-version":"2099-01-01","mcp-method":"tools/list"})
add("m claim 2099 no headers", req(1,"tools/list",{"_meta":dict(M,**{"io.modelcontextprotocol/protocolVersion":"2099-01-01"})}))
add("m claim classic", req(1,"tools/list",{"_meta":dict(M,**{"io.modelcontextprotocol/protocolVersion":"2025-06-18"})}), {"mcp-protocol-version":"2025-06-18","mcp-method":"tools/list"})
add("m claim 5", req(1,"tools/list",{"_meta":dict(M,**{"io.modelcontextprotocol/protocolVersion":5})}), mh("tools/list"))
add("m no capabilities", req(1,"tools/list",{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}), mh("tools/list"))
add("m capabilities array", req(1,"tools/list",{"_meta":dict(M,**{"io.modelcontextprotocol/clientCapabilities":[]})}), mh("tools/list"))
add("m clientInfo bad", req(1,"tools/list",{"_meta":dict(M,**{"io.modelcontextprotocol/clientInfo":{"name":5}})}), mh("tools/list"))
add("m progressToken float", req(1,"tools/list",{"_meta":dict(M,progressToken=1.5)}), mh("tools/list"))
add("m ping", req(1,"ping",env()), mh("ping"))
add("m initialize", req(1,"initialize",env()), mh("initialize"))
add("m accept json only", req(1,"tools/list",env()), dict(mh("tools/list"),accept="application/json"))
add("m accept none", req(1,"tools/list",env()), mh("tools/list"), drop=("accept",))
add("m no/such", req(1,"no/such",env()), mh("no/such"))
add("m string id", req("s","tools/list",env()), mh("tools/list"))
# methods
for m in ["GET","DELETE","PUT","PATCH","OPTIONS"]:
    add("http "+m, "", method=m)
add("deep nesting", '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":' + "["*10001 + "]"*10001 + "}}")
json.dump(cases, open(OUT,"w"), ensure_ascii=False, indent=2); open(OUT,"a").write("\n")
print(len(cases))
