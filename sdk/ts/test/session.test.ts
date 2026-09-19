import { expect, test } from "bun:test";
import { DirectSession, sha256 } from "../src/utils";
import { Filegate } from "../src/index";

const status = {id:"session",root:"test",state:"open",size:3,chunkSize:1,received:0,uploadedSegments:0};

test("DirectSession uses native Bun fetch and resumes seek-paged receipts", async () => {
  const requests: {method:string;after:string|null}[] = [];
  const hashes = await Promise.all(["a","c"].map(v=>sha256(new TextEncoder().encode(v))));
  const server=Bun.serve({port:0,fetch:async request=>{
    expect(request.headers.has("authorization")).toBe(false);
    const url=new URL(request.url);requests.push({method:request.method,after:url.searchParams.get("after")});
    if(url.searchParams.has("segments")){
      return Response.json(url.searchParams.get("after")==="-1"?{items:[{index:0,hash:hashes[0]}],next:0}:{items:[{index:2,hash:hashes[1]}]});
    }
    if(request.method==="PUT") {expect(url.searchParams.get("segment")).toBe("1");expect(await request.text()).toBe("b");return Response.json({...status,received:3,uploadedSegments:3});}
    return Response.json({...status,received:2,uploadedSegments:2});
  }});
  try {
    expect(await new DirectSession(server.url.href).upload(new Blob(["abc"]))).toMatchObject({received:3,uploadedSegments:3});
    expect(requests).toEqual([{method:"GET",after:null},{method:"GET",after:"-1"},{method:"GET",after:"0"},{method:"PUT",after:null}]);
  } finally {await server.stop(true);}
});

test("fetch receives its global receiver, transient retries are bounded",async()=>{
 let calls=0;
 const request:typeof fetch=async function(this:unknown){expect(this).toBe(globalThis);calls++;return Response.json({error:"busy"},{status:503,headers:{"Retry-After":"0"}});};
 await expect(new DirectSession("https://file.test/lease",request).status()).rejects.toMatchObject({status:503});
 expect(calls).toBe(3);
});

test("idempotent segment retry uses identical bytes and no commit",async()=>{
 let calls=0;
 const request:typeof fetch=async (_url,init)=>{
  expect(init?.method).toBe("PUT");expect(init?.credentials).toBe("omit");expect(await new Response(init?.body).text()).toBe("abc");calls++;
  if(calls===1)return new Response(null,{status:503,headers:{"Retry-After":"0"}});
  return Response.json({...status,received:3,uploadedSegments:1});
 };
 expect(await new DirectSession("https://file.test/lease",request).put(0,new Blob(["abc"]))).toMatchObject({received:3});
 expect(calls).toBe(2);
});

test("abort interrupts retry backoff and avoids another request",async()=>{
 const controller=new AbortController();let calls=0;
 const request:typeof fetch=async()=>{calls++;queueMicrotask(()=>controller.abort(new Error("cancelled")));return new Response(null,{status:503,headers:{"Retry-After":"2"}});};
 await expect(new DirectSession("https://file.test/lease",request).status(controller.signal)).rejects.toThrow("cancelled");
 expect(calls).toBe(1);
});

test("permission failures and abort are not retried",async()=>{
 for(const operation of ["status","abort"] as const){let calls=0;const request:typeof fetch=async()=>{calls++;return Response.json({error:"denied"},{status:operation==="status"?403:503});};
  await expect(new DirectSession("https://file.test/lease",request)[operation]()).rejects.toBeInstanceOf(Error);expect(calls).toBe(1);
 }
});

test("session creation binds key while lease renewal remains independent",async()=>{
 const calls:{url:string;body:unknown}[]=[];
 const request:typeof fetch=async (url,init)=>{calls.push({url:String(url),body:init?.body?JSON.parse(String(init.body)):undefined});return Response.json({session:status});};
 const root=new Filegate({baseUrl:"https://file.test",token:"backend",fetch:request}).root("test");
 await root.createSession("file",3,{idempotencyKey:"retry-key",expiresIn:30});
 await root.sessionSegments("session",7,50);
 expect(calls[0].body).toEqual({path:"file",size:3,idempotencyKey:"retry-key",expiresIn:30});
 expect(calls[1].url).toBe("https://file.test/v1/roots/test/uploads/sessions/session/segments?after=7&limit=50");
});

test("long Retry-After does not trigger an early automatic retry",async()=>{
 let calls=0;
 const request:typeof fetch=async()=>{calls++;return Response.json({error:"busy"},{status:429,headers:{"Retry-After":"60"}});};
 await expect(new DirectSession("https://file.test/lease",request).status()).rejects.toMatchObject({status:429});
 expect(calls).toBe(1);
});
