// Run with NODE_PATH pointing to a Playwright installation.
const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'workbuddy/unified_panel.html'));
const child = fs.readFileSync(path.join(root, 'workbuddy/panel.html'));
const accounts = [{auth_index:'tr-a',uid:'a',name:'<img src=x onerror=alert(1)>',remain:1234,used:20,disabled:false,checked_in:false,checkin_enabled:true}];
let toggleCalls=0, saved=null, authenticated=0;
const server=http.createServer(async(req,res)=>{
  const url=new URL(req.url,'http://localhost');
  const send=(status,data)=>{res.writeHead(status,{'Content-Type':'application/json'});res.end(JSON.stringify(data))};
  if(url.pathname==='/v0/resource/plugins/workbuddy/panel'){res.writeHead(200,{'Content-Type':'text/html'});return res.end(html)}
  if(url.pathname==='/v0/resource/plugins/workbuddy/codebuddy'){res.writeHead(200,{'Content-Type':'text/html'});return res.end(child)}
  if(req.headers.authorization!=='Bearer fixture-key')return send(401,{error:'unauthorized'});
  authenticated++;
  let raw='';for await(const chunk of req)raw+=chunk;const body=raw?JSON.parse(raw):{};
  const route=url.pathname.replace('/v0/management/plugins/workbuddy','');
  if(route==='/combined/accounts')return send(200,{version:'unified-test',codebuddy:{accounts:[{region:'global',credits:{total_remain:100}},{region:'cn'}]},trae:{accounts}});
  if(route==='/trae/accounts')return send(200,{accounts});
  if(route==='/trae/models')return send(200,{models:[{ID:'trae/first'},{ID:'trae/second'}],disabled_models:['second']});
  if(route==='/trae/models/config'){saved=body;return send(200,{saved:true,reload:{status:'ok'}})}
  if(route==='/trae/toggle'){toggleCalls++;await new Promise(r=>setTimeout(r,500));accounts[0].disabled=!body.enabled;return send(200,{enabled:body.enabled})}
  if(route==='/trae/import'){assert.equal(body.type,'trae');return send(200,{uid:'b'})}
  if(route==='/trae/login/start')return send(200,{State:'fixture-state',URL:'https://example.test/login'});
  if(route==='/trae/login/poll')return send(200,{Status:'pending'});
  if(route==='/combined/usage')return send(200,{records:[{channel:'trae',timestamp:'2026-10-18T01:00:00Z',model:'trae/first',input_tokens:2},{region:'cn',timestamp:'2026-10-18T01:00:00Z',model:'cb-model',input_tokens:3}]});
  if(route==='/accounts')return send(200,{accounts:[]});
  if(route==='/update/check')return send(200,{available:false});
  return send(200,{});
});
(async()=>{
  await new Promise(r=>server.listen(0,'127.0.0.1',r));
  const origin='http://127.0.0.1:'+server.address().port;
  const browser=await chromium.launch({headless:true});
  try{
    const page=await browser.newPage({viewport:{width:1280,height:900},locale:'zh-CN'});
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.goto(origin+'/v0/resource/plugins/workbuddy/panel?key=fixture-key');await page.waitForLoadState('networkidle');
    assert.equal(await page.locator('#metrics .metric').count(),3);
    assert.equal(new URL(page.url()).searchParams.has('key'),false);
    assert.equal(await page.evaluate(()=>sessionStorage.getItem('workbuddy-mgmt-key')),'fixture-key');
    assert.equal(await page.evaluate(()=>JSON.stringify(localStorage).includes('fixture-key')),false);
    await page.locator('[data-tab=trae]').click();await page.waitForLoadState('networkidle');
    assert.equal(await page.locator('#traeBody img').count(),0);
    await page.locator('[data-models]').click();await page.waitForSelector('[data-model=first]');
    await page.locator('[data-model=first]').uncheck();
    await page.locator('#panelLanguage').selectOption('en');
    assert.equal(await page.locator('[data-model=first]').isChecked(),false);
    assert.equal(await page.locator('[data-model=second]').isChecked(),false);
    await page.locator('#saveModels').click();await page.waitForFunction(()=>!document.getElementById('modelsDialog').open);
    assert.deepEqual(saved,{auth_index:'tr-a',disabled_models:['second','first']});
    await page.locator('[data-toggle]').click();await page.locator('#panelLanguage').selectOption('zh-CN');
    assert.equal(await page.locator('[data-toggle]').isDisabled(),true);
    await page.waitForFunction(()=>document.querySelector('[data-toggle]')&&!document.querySelector('[data-toggle]').disabled);
    assert.equal(toggleCalls,1);
    await page.locator('#traeImport').click();await page.locator('#importRaw').fill('{"type":"trae","auth":{"accessToken":"fixture"}}');
    await page.locator('#importForm button[type=submit]').click();await page.waitForFunction(()=>!document.getElementById('importDialog').open);
    assert.equal(await page.locator('#importRaw').inputValue(),'');
    await page.locator('[data-tab=usage]').click();await page.waitForFunction(()=>document.querySelector('#usageBody').textContent.includes('cb-model'));
    assert.equal(await page.locator('#usageBody tr').count(),2);
    await page.locator('#usageChannel').selectOption('trae');assert.equal(await page.locator('#usageBody tr').count(),1);
    await page.locator('[data-tab=overview]').click();await page.waitForLoadState('networkidle');
    const out=path.join(root,'workbuddy/dist/ui');fs.mkdirSync(out,{recursive:true});
    await page.screenshot({path:path.join(out,'unified-light.png'),fullPage:true});
    await page.evaluate(()=>document.documentElement.setAttribute('data-theme','dark'));
    await page.screenshot({path:path.join(out,'unified-dark.png'),fullPage:true});
    await page.setViewportSize({width:390,height:844});await page.screenshot({path:path.join(out,'unified-mobile.png'),fullPage:true});
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'mobile page overflows');
    await page.setViewportSize({width:1280,height:900});await page.locator('[data-tab=codebuddy]').click();await page.frameLocator('#codebuddyFrame').locator('#panelLanguage').waitFor();
    assert.equal(await page.frameLocator('#codebuddyFrame').locator('#panelLanguage').count(),1);
    await page.locator('#panelLanguage').selectOption('en');
    assert.equal(await page.frameLocator('#codebuddyFrame').locator('#panelLanguage').inputValue(),'en');
    await page.locator('[data-tab=trae]').click();await page.locator('#traeLogin').click();await page.waitForSelector('#loginBox:not([hidden])');
    await page.locator('#cancelLogin').click();assert.equal(await page.locator('#loginBox').isHidden(),true);
    assert.deepEqual(errors,[]);
    const bad=await browser.newPage();await bad.goto(origin+'/v0/resource/plugins/workbuddy/panel?key=wrong');await bad.waitForLoadState('networkidle');
    assert.equal(await bad.locator('#authBox').isVisible(),true);
    const before=authenticated;await bad.locator('[data-tab=trae]').click();assert.equal(authenticated,before);
    console.log('PASS: bilingual tabs, iframe, model drafts/save, credential import, pending-action isolation, usage filters, auth failure, dark/mobile layouts.');
  }finally{await browser.close();server.close()}
})().catch(e=>{console.error(e);server.close();process.exitCode=1});
