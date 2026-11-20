const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync,execFileSync} = require('node:child_process');

for (const managed of [false,true]) for (const compatible of [false,true]) {
 test(`setup validates its selected SDK: managed=${managed} compatible=${compatible}`, () => {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(),'tags-selected-sdk-'));
  try {
   const source = path.join(temp,'plugin');
   const remote = path.join(temp,'remote');
   const external = path.join(temp,'external/pluginsdk');
   const bin = path.join(temp,'bin');
   const root = path.join(source,'.build/kandev');
   for (const dir of [source,remote,external,bin]) fs.mkdirSync(dir,{recursive:true});
   fs.mkdirSync(path.join(remote,'pluginsdk'));
   fs.mkdirSync(path.join(remote,'apps/backend/cmd/plugin-pack'),{recursive:true});
   fs.writeFileSync(path.join(remote,'pluginsdk/go.mod'),'module example.com/sdk\n');
   fs.writeFileSync(path.join(remote,'apps/backend/cmd/plugin-pack/main.go'),'package main\n');
   if (compatible) fs.writeFileSync(path.join(remote,'pluginsdk/compatible'),'supported API');
   fs.writeFileSync(path.join(external,'go.mod'),'module example.com/sdk\n');
   fs.writeFileSync(path.join(external,'compatible'),'supported API');
   const git = (...args) => execFileSync('git',args,{cwd:remote,encoding:'utf8'}).trim();
   git('init','-q'); git('add','.');
   git('-c','user.name=Test','-c','user.email=test@example.com','-c','commit.gpgsign=false','commit','-qm','fixture');
   const revision = git('rev-parse','HEAD');
   for (const file of ['Makefile','go.mod']) fs.copyFileSync(path.join(__dirname,file),path.join(source,file));
   if (managed) {
    fs.mkdirSync(path.dirname(root),{recursive:true});
    git('clone','-q',remote,root);
    fs.writeFileSync(path.join(root,'.git/kandev-plugin-tags-sdk'),revision+'\n');
   }
   const selected = path.join(temp,'selected-sdk');
   fs.writeFileSync(path.join(bin,'go'),`#!/bin/sh
if [ "$1" = mod ]; then
 for arg do case "$arg" in github.com/kdlbs/kandev/pluginsdk=*) printf '%s' "\${arg#*=}" > '${selected}';; esac; done
 exit 0
fi
SDK=$(cat '${selected}')
if [ ! -f "$SDK/compatible" ]; then echo 'selected SDK lacks required API' >&2; exit 1; fi
`,{mode:0o755});
   const env = {...process.env,PATH:bin+path.delimiter+process.env.PATH};
   for (const key of ['MAKEFLAGS','MFLAGS','MAKELEVEL','MAKEOVERRIDES','KANDEV_ROOT','KANDEV_SDK','KANDEV_BACKEND','GO_MOD_KANDEV_SDK','SETUP_KANDEV_REPO','_SDK_SETUP_OWNER']) delete env[key];
   const result = spawnSync('make',['--no-print-directory','setup',`KANDEV_REPOSITORY=${remote}`,`KANDEV_REVISION=${revision}`,`KANDEV_SDK=${external}`],{cwd:source,env,encoding:'utf8'});
   assert.equal(fs.readFileSync(selected,'utf8'),path.join(root,'pluginsdk'),'setup compiled the external override instead of its selected checkout');
   if (compatible) {
    assert.equal(result.status,0,result.stdout+result.stderr);
    assert.equal(fs.readFileSync(path.join(root,'.git/kandev-plugin-tags-sdk'),'utf8').trim(),revision);
    assert.equal(fs.existsSync(path.join(root,'.kandev-plugin-tags-sdk-setup')),false);
   } else {
    assert.notEqual(result.status,0,'incompatible selected checkout was marked validated');
    assert.match(result.stderr,/selected SDK lacks required API/);
    assert.equal(fs.existsSync(root),managed,'failure removed an existing checkout or retained an owned cold reservation');
    if (managed) assert.equal(fs.readFileSync(path.join(root,'.git/kandev-plugin-tags-sdk'),'utf8').trim(),revision);
   }
  } finally {fs.rmSync(temp,{recursive:true,force:true});}
 });
}
