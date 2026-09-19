import { expect, test } from "bun:test";
import { Filegate } from "../src/index";

test("internal transfer origin is explicit, scoped, and does not receive backend credentials",async()=>{
 const calls:{url:string;authorization:string|null}[]=[];
 const publicURL="https://public.test/v1/direct/payload.signature";
 const request:typeof fetch=async(input,init)=>{
  const url=String(input);calls.push({url,authorization:new Headers(init?.headers).get("Authorization")});
  if(url.includes("/uploads/direct"))return Response.json({url:publicURL,method:"PUT",expires:"later"});
  return Response.json({path:"file"});
 };
 const files=new Filegate({baseUrl:"https://api.test",transferBaseUrl:"http://internal.test:9090",token:"backend",fetch:request});
 const lease=await files.root("test").directUpload("file",3);
 expect(lease.url).toBe(publicURL);
 await files.root("test").put("file",new Blob(["abc"]));
 expect(calls.at(-1)).toEqual({url:"http://internal.test:9090/v1/direct/payload.signature",authorization:null});
 const response=await files.downloadRaw({url:publicURL,method:"GET",expires:"later"});expect(response.status).toBe(200);
 const session=files.directSession({url:publicURL,expires:"later",operations:["status"]});
 expect(session.url).toBe("http://internal.test:9090/v1/direct/payload.signature");
 for(const url of ["https://public.test/admin","https://user:pass@public.test/v1/direct/payload.signature","https://public.test/v1/direct/payload.signature#fragment","https://public.test/v1/direct/payload%2fsignature"]){expect(()=>files.transferUrl(url)).toThrow();}
 expect(()=>new Filegate({baseUrl:"https://api.test",transferBaseUrl:"http://internal.test/path",token:"backend"})).toThrow();
});

test("query options, conditional publication, and signed download names stay typed",async()=>{
 const calls:{url:string;body:unknown}[]=[];
 const request:typeof fetch=async(input,init)=>{calls.push({url:String(input),body:init?.body?JSON.parse(String(init.body)):undefined});return Response.json({});};
 const root=new Filegate({baseUrl:"https://api.test",token:"backend",fetch:request}).root("test");
 await root.list("folder",{sort:"size",order:"desc",type:"directories",limit:20});
 await root.search("note",{path:"folder",sort:"modified",order:"asc",type:"files",after:"cursor"});
 await root.directUpload("file",3,{onConflict:"overwrite",precondition:{ifMatch:"revision"}});
 await root.directDownload("file",{fileName:"Grüße.txt",expiresIn:30});
 await root.directVersionDownload("file","version",{fileName:"Previous.txt"});
 await root.recursiveStats("folder",12);
 expect(new URL(calls[0].url).searchParams.get("type")).toBe("directories");
 expect(new URL(calls[1].url).searchParams.get("sort")).toBe("modified");
 expect(calls[2].body).toMatchObject({precondition:{ifMatch:"revision"}});
 expect(calls[3].body).toEqual({path:"file",fileName:"Grüße.txt",expiresIn:30});
 expect(calls[4].body).toEqual({path:"file",fileName:"Previous.txt"});
 expect(new URL(calls[5].url).searchParams.get("maxEntries")).toBe("12");
});

test("transfer recovery stays explicit and historical copies use their own endpoint",async()=>{
 const calls:{url:string;body:unknown}[]=[];
 const request:typeof fetch=async(input,init)=>{calls.push({url:String(input),body:init?.body?JSON.parse(String(init.body)):undefined});return Response.json({state:"source_pending",id:"transfer"},{status:202});};
 const root=new Filegate({baseUrl:"https://api.test",token:"backend",fetch:request}).root("test");
 expect(await root.transfer("source","other","target",{move:true,id:"transfer"})).toMatchObject({state:"source_pending"});
 expect(calls).toHaveLength(1);
 await root.transferStatus("transfer");await root.resumeTransfer("transfer");await root.abandonTransfer("transfer");
 await root.copyVersion("source","version","other","target",{ownership:{mode:"0640"}});
 expect(calls.map(call=>new URL(call.url).pathname)).toEqual(["/v1/roots/test/transfers","/v1/roots/test/transfers/transfer","/v1/roots/test/transfers/transfer/resume","/v1/roots/test/transfers/transfer/abandon","/v1/roots/test/versions/version/copy"]);
 expect(calls[4].body).toEqual({path:"source",targetRoot:"other",targetPath:"target",ownership:{mode:"0640"}});
});
