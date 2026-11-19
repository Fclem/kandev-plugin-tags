const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawn, spawnSync, execFileSync} = require('node:child_process');

function fixture(barrier = 'checkout') {
 const temp = fs.mkdtempSync(path.join(os.tmpdir(),'tags-setup-race-'));
 const remote = path.join(temp,'remote');
 const source = path.join(temp,'plugin');
 const bin = path.join(temp,'bin');
 for (const dir of [remote,source,bin]) fs.mkdirSync(dir);
 fs.mkdirSync(path.join(remote,'pluginsdk'));
 fs.mkdirSync(path.join(remote,'apps/backend/cmd/plugin-pack'),{recursive:true});
 fs.writeFileSync(path.join(remote,'pluginsdk/go.mod'),'module example.com/sdk\n');
 fs.writeFileSync(path.join(remote,'apps/backend/cmd/plugin-pack/main.go'),'package main\n');
 const realGit = execFileSync('sh',['-c','command -v git'],{encoding:'utf8'}).trim();
 const git = (...args) => execFileSync(realGit,args,{cwd:remote,encoding:'utf8'}).trim();
 git('init','-q'); git('add','.');
 const commit = () => git('-c','user.name=Test','-c','user.email=test@example.com','-c','commit.gpgsign=false','commit','-qam','fixture');
 commit(); const firstRevision = git('rev-parse','HEAD');
 fs.writeFileSync(path.join(remote,'pluginsdk/version.txt'),'second revision\n');
 git('add','.'); commit(); const secondRevision = git('rev-parse','HEAD');
 for (const file of ['Makefile','go.mod']) fs.copyFileSync(path.join(__dirname,file),path.join(source,file));
 const ready = path.join(temp,'ready');
 const gate = path.join(temp,'gate');
 execFileSync('mkfifo',[gate]);
 fs.writeFileSync(path.join(bin,'git'),`#!/bin/sh\nfor arg do\n if [ "$arg" = checkout ] && [ "$BLOCK_SETUP" = 1 ] && [ '${barrier}' = checkout ]; then touch '${ready}'; read release < '${gate}'; fi\ndone\nexec '${realGit}' "$@"\n`,{mode:0o755});
 fs.writeFileSync(path.join(bin,'go'),`#!/bin/sh\nif [ "$1" = test ] && [ "$BLOCK_SETUP" = 1 ] && [ '${barrier}' = compile ]; then touch '${ready}'; read release < '${gate}'; fi\nexit 0\n`,{mode:0o755});
 const env = {...process.env,PATH:bin+path.delimiter+process.env.PATH};
 for (const key of ['MAKEFLAGS','MFLAGS','MAKELEVEL','MAKEOVERRIDES','KANDEV_ROOT','KANDEV_SDK','KANDEV_BACKEND','GO_MOD_KANDEV_SDK','SETUP_KANDEV_REPO']) delete env[key];
 const args = revision => ['--no-print-directory','setup',`KANDEV_REPOSITORY=${remote}`,`KANDEV_REVISION=${revision}`];
 const start = () => {
  const child = spawn('make',args(firstRevision),{cwd:source,env:{...env,BLOCK_SETUP:'1'},detached:true});
  let output = ''; child.stdout.on('data',b => output+=b); child.stderr.on('data',b => output+=b);
  const deadline = setTimeout(() => {try {process.kill(-child.pid,'SIGKILL');} catch {}},15000);
  const done = new Promise(resolve => child.once('close',status => {clearTimeout(deadline);resolve({status,output});}));
  const pending = new Promise((resolve,reject) => {
   const watcher = fs.watch(temp,() => {if(fs.existsSync(ready)){clearTimeout(timer);watcher.close();resolve();}});
   const timer = setTimeout(() => {watcher.close();reject(new Error('setup did not reach '+barrier+' barrier: '+output));},10000);
   if(fs.existsSync(ready)){clearTimeout(timer);watcher.close();resolve();}
  });
  return {child,done,pending};
 };
 return {temp,source,remote,root:path.join(source,'.build/kandev'),realGit,firstRevision,secondRevision,start,
  release:() => fs.writeFileSync(gate,'continue\n'),
  second:(...extra) => spawnSync('make',[...args(secondRevision),...extra],{cwd:source,env,encoding:'utf8'})};
}

test('concurrent fresh setup cannot publish or compile the other requested revision',async () => {
 const f = fixture(); const first = f.start();
 try {
  await first.pending;
  const second = f.second();
  f.release(); const result = await first.done;
  assert.equal(result.status,0,result.output);
  assert.notEqual(second.status,0,second.stdout+second.stderr);
  assert.equal(execFileSync(f.realGit,['-C',f.root,'rev-parse','HEAD'],{encoding:'utf8'}).trim(),f.firstRevision);
  assert.deepEqual(fs.readdirSync(f.root).filter(name => name.includes('.setup.')),[]);
 } finally {try {process.kill(-first.child.pid,'SIGKILL');} catch {} fs.rmSync(f.temp,{recursive:true,force:true});}
});

test('a checkout destination cannot be taken over while setup fetches',async () => {
 const f = fixture(); const first = f.start();
 try {
  await first.pending;
  const reserved = fs.existsSync(f.root);
  if (!reserved) {fs.mkdirSync(f.root,{recursive:true});fs.writeFileSync(path.join(f.root,'keep'),'user checkout');}
  f.release(); const result = await first.done;
  if (reserved) {
   assert.equal(result.status,0,result.output);
   assert.equal(execFileSync(f.realGit,['-C',f.root,'rev-parse','HEAD'],{encoding:'utf8'}).trim(),f.firstRevision);
  } else {
   assert.notEqual(result.status,0,result.output);
   assert.deepEqual(fs.readdirSync(f.root),['keep']);
   assert.equal(fs.readFileSync(path.join(f.root,'keep'),'utf8'),'user checkout');
  }
 } finally {try {process.kill(-first.child.pid,'SIGKILL');} catch {} fs.rmSync(f.temp,{recursive:true,force:true});}
});

test('managed setup stays private through validation and never reuses a different requested pin',async () => {
 const f = fixture('compile'); const first = f.start();
 try {
  await first.pending;
  assert.equal(execFileSync(f.realGit,['-C',f.root,'rev-parse','HEAD'],{encoding:'utf8'}).trim(),f.firstRevision);
  const second = f.second();
  f.release(); const result = await first.done;
  assert.equal(result.status,0,result.output);
  assert.notEqual(second.status,0,'another pin succeeded while validation was pending: '+second.stdout+second.stderr);
  const reused = f.second();
  assert.notEqual(reused.status,0,'managed reuse silently accepted a different revision: '+reused.stdout+reused.stderr);
  assert.equal(execFileSync(f.realGit,['-C',f.root,'rev-parse','HEAD'],{encoding:'utf8'}).trim(),f.firstRevision);
  const explicit = path.join(f.temp,'explicit');
  execFileSync(f.realGit,['clone','-q',f.remote,explicit]);
  execFileSync(f.realGit,['-C',explicit,'checkout','-q','--detach',f.firstRevision]);
  fs.writeFileSync(path.join(explicit,'keep'),'user edits');
  const accepted = f.second(`KANDEV_ROOT=${explicit}`);
  assert.equal(accepted.status,0,accepted.stdout+accepted.stderr);
  assert.equal(execFileSync(f.realGit,['-C',explicit,'rev-parse','HEAD'],{encoding:'utf8'}).trim(),f.firstRevision);
  assert.equal(fs.readFileSync(path.join(explicit,'keep'),'utf8'),'user edits');
 } finally {try {process.kill(-first.child.pid,'SIGKILL');} catch {} fs.rmSync(f.temp,{recursive:true,force:true});}
});
