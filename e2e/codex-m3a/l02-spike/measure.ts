import assert from 'node:assert/strict';
import {FakeProvider,dummyCredential} from '/work/e2e/codex-m3a/fake-provider.ts';
import {SupervisorRoot,codeModeHostConfigToml} from './supervisor-driver.ts';
import {message,callOutput} from '/work/e2e/codex-m0/harness.mjs';

// Scratch-only extension of the existing lifecycle fixture. No real Bash process,
// credentials, network access outside loopback, background command, or native tool
// authority: the only dynamic effect is a fixed-duration promise callback.
const holdMs=45_000;
for(const variant of [
  {name:'default-exec-and-wait',exec:undefined,wait:undefined},
  {name:'long-exec',exec:120_000,wait:120_000},
  {name:'default-wait',exec:1,wait:undefined},
  {name:'long-wait',exec:1,wait:120_000},
]){
  const credential=dummyCredential();
  const root=`/data/runner/l02-${variant.name}`;
  const origin=performance.now();
  const requests:{atMs:number;reply:string;output?:string}[]=[];
  let enteredAt:number|undefined,settledAt:number|undefined,cellId:string|undefined;
  let lastCall='initial-cell';
  const provider=await FakeProvider.start({credential,respond:async(body)=>{
    const atMs=performance.now()-origin;
    if(requests.length===0){
      requests.push({atMs,reply:'exec'});
      return [{type:'custom_tool_call',name:'exec',call_id:lastCall,input:
        `${variant.exec===undefined?'':`// @exec: {"yield_time_ms": ${variant.exec}}\n`}text(await tools.uzi_bash({command:"harmless fixed callback fixture"}));`}];
    }
    const result=callOutput(body,lastCall);
    assert.ok(result,`missing output for ${lastCall}`);
    const output=JSON.stringify(result);
    const match=/Script running with cell ID\s+([^\s"\\]+)/.exec(output);
    if(match){
      cellId=match[1];
      requests.push({atMs,reply:'wait',output:output.slice(0,220)});
      lastCall=`wait-${requests.length}`;
      return [{type:'function_call',name:'wait',call_id:lastCall,arguments:JSON.stringify({cell_id:cellId,...(variant.wait===undefined?{}:{yield_time_ms:variant.wait})})}];
    }
    assert.ok(settledAt!==undefined,`unexpected pre-completion output: ${output}`);
    assert.ok(output.includes('Script completed') && output.includes('held callback completed'),
      `missing successful callback result: ${output.slice(0,1000)}`);
    requests.push({atMs,reply:'finish',output:output.slice(0,220)});
    return [message()];
  }});
  const env={HOME:root+'/home',CODEX_HOME:root+'/codex',XDG_CONFIG_HOME:root+'/xdg-config',XDG_CACHE_HOME:root+'/xdg-cache',XDG_DATA_HOME:root+'/xdg-data',XDG_STATE_HOME:root+'/xdg-state',TMPDIR:root+'/tmp',PATH:'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/uzi-toolchain/bin',SHELL:'/bin/sh',LANG:'C',TERM:'dumb',FAKE_PROVIDER_API_KEY:credential};
  const sup=await SupervisorRoot.start({supervisorBin:'/usr/local/bin/uzi-codex-supervisor',codexBin:'/opt/uzi-codex/0.160.1/bin/codex',childArgv:['app-server'],ownedDataRoot:root,cwd:root+'/cwd',expectUid:10002,configToml:codeModeHostConfigToml({model:'gpt-6-astra',baseUrl:provider.baseUrl,envKey:'FAKE_PROVIDER_API_KEY',projectPath:root+'/cwd'}),env,
    dynamicTool:async(request)=>{
      assert.equal(request.params.tool,'uzi_bash');
      enteredAt=performance.now()-origin;
      await new Promise(resolve=>setTimeout(resolve,holdMs));
      settledAt=performance.now()-origin;
      return {result:{success:true,contentItems:[{type:'inputText',text:'held callback completed'}]}};
    }});
  try{
    await sup.initialize();
    const id=await sup.threadStart({model:'gpt-6-astra',environments:[],approvalPolicy:'never',dynamicTools:[{name:'uzi_bash',description:'Harmless fixed-duration Bash callback fixture',inputSchema:{type:'object',properties:{command:{type:'string'}},required:['command'],additionalProperties:false}}]});
    await sup.turnStart(id);
    const completed=await sup.until(()=>sup.messages.find((m:any)=>m.method==='turn/completed'),'turn completed',75_000);
    const turn=(completed.params as {turn?:{status?:string;error?:unknown}})?.turn;
    assert.equal(turn?.status,'completed',`turn was not successful: ${JSON.stringify(turn).slice(0,1000)}`);
    assert.equal(turn?.error ?? null,null);
    assert.ok(enteredAt!==undefined && settledAt!==undefined);
    assert.deepEqual(provider.errors,[]);
    assert.deepEqual(sup.errors,[]);
    const disposal=await sup.dispose();
    assert.equal(disposal.state,'drained');
    assert.equal(disposal.authority,'ECHILD+__WALL');
    console.log(JSON.stringify({variant,holdMs,enteredAt,settledAt,providerRequests:requests.length,requestsDuringPending:requests.filter(r=>r.atMs>=enteredAt!&&r.atMs<settledAt!).length,completionWakeMs:requests.at(-1)!.atMs-settledAt,requests,disposal}));
  }finally{await sup.close();await provider.close();}
}
