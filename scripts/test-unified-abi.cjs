const fs=require('node:fs'),path=require('node:path'),os=require('node:os'),assert=require('node:assert/strict'),{spawnSync}=require('node:child_process');
const root=path.resolve(__dirname,'..'),dist=path.join(root,'workbuddy/dist/windows/amd64');
const fixture=fs.mkdtempSync(path.join(os.tmpdir(),'unified-abi-'));
const encoded=value=>Buffer.from(typeof value==='string'?value:JSON.stringify(value)).toString('base64');
function call(method,request,expectError=false){
  const input=path.join(fixture,'input.json'),events=path.join(fixture,'events.jsonl');fs.writeFileSync(input,JSON.stringify(request));
  const p=spawnSync(path.join(dist,'abi-unified-smoke.exe'),[path.join(dist,'workbuddy.dll'),method,input,events],{encoding:'utf8',timeout:30000,env:{...process.env,WORKBUDDY_DATA_DIR:fixture}});
  if(p.error)throw p.error;
  if(expectError){assert.notEqual(p.status,0);assert.equal(JSON.parse(p.stdout).ok,false);return}
  assert.equal(p.status,0,method+': '+p.stderr+' '+p.stdout);
  const env=JSON.parse(p.stdout);assert.equal(env.ok,true);return {result:env.result,events:fs.readFileSync(events,'utf8')};
}
for(const channel of ['codebuddy','trae']){
  const trae=channel==='trae';const credential={type:'workbuddy',...(trae?{channel:'trae'}:{}),disabled_models:['hidden'],custom: 'retained',auth:{accessToken:'fixture',refreshToken:'refresh',domain:trae?'trae.cn':'www.codebuddy.cn',expiresAt:4102444800,deviceId:'1234567890123456',machineId:'machine'},account:{uid:channel,nickname:channel}};
  const storage=encoded(credential),model=trae?'trae/fixture-model':'fixture-model';
  const parsed=call('auth.parse',{Provider:'workbuddy',FileName:(trae?'trae-':'workbuddy-')+channel+'.json',RawJSON:storage});
  assert.equal(parsed.result.Handled,true);assert.equal(parsed.result.Auth.Provider,'workbuddy');
  if(trae){const discovered=call('model.for_auth',{AuthID:channel,AuthProvider:'workbuddy',StorageJSON:storage});assert.equal(discovered.result.Models[0].ID,model)}
  const body={model,messages:[{role:'user',content:'Say OK'}]};
  const req={AuthID:channel,AuthProvider:'workbuddy',StorageJSON:storage,Model:model,Format:'openai',SourceFormat:'openai',Payload:encoded(body),Metadata:{request_path:'/v1/chat/completions'}};
  const completed=call('executor.execute',req);const payload=JSON.parse(Buffer.from(completed.result.Payload,'base64').toString());assert.match(payload.choices[0].message.content,/fixture answer/);
  const streamed=call('executor.execute_stream',{...req,stream_id:'fixture-stream'});assert.match(streamed.events,/payload/);assert.doesNotMatch(streamed.events,/"error"/);
  call('executor.execute',{...req,Model:trae?'fixture-model':'trae/fixture-model'},true);
}
console.log('PASS: real Windows DLL parses both providers, discovers prefixed Trae models, completes and streams both protocols, rejects crossed credentials.');
