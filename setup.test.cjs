const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync, execFileSync} = require('node:child_process');

test('default setup recovers a failed fetch without resetting explicit checkouts', () => {
 const temp = fs.mkdtempSync(path.join(os.tmpdir(),'tags-setup-'));
 try {
  const remote = path.join(temp,'remote');
  const source = path.join(temp,'plugin');
  const bin = path.join(temp,'bin');
  for (const dir of [remote,source,bin]) fs.mkdirSync(dir);
  fs.mkdirSync(path.join(remote,'pluginsdk'));
  fs.writeFileSync(path.join(remote,'pluginsdk/go.mod'),'module example.com/sdk\n');
  fs.mkdirSync(path.join(remote,'apps/backend/cmd/plugin-pack'),{recursive:true});
  fs.writeFileSync(path.join(remote,'apps/backend/cmd/plugin-pack/main.go'),'package main\n');
  const realGit = execFileSync('sh',['-c','command -v git'],{encoding:'utf8'}).trim();
  const git = (...args) => execFileSync(realGit,args,{cwd:remote,encoding:'utf8'}).trim();
  git('init','-q'); git('add','.');
  git('-c','user.name=Test','-c','user.email=test@example.com','-c','commit.gpgsign=false','commit','-qm','fixture');
  const revision = git('rev-parse','HEAD');
  for (const file of ['Makefile','go.mod']) fs.copyFileSync(path.join(__dirname,file),path.join(source,file));
  const marker = path.join(temp,'failed-once');
  fs.writeFileSync(path.join(bin,'git'),`#!/bin/sh\ncase "$*" in *fetch*) if [ ! -e '${marker}' ]; then touch '${marker}'; echo simulated-fetch-outage >&2; exit 1; fi;; esac\nexec '${realGit}' "$@"\n`,{mode:0o755});
  const log = path.join(temp,'go.log');
  fs.writeFileSync(path.join(bin,'go'),`#!/bin/sh\nprintf '%s\\n' "$*" >> '${log}'\n`,{mode:0o755});
  const env = {...process.env,PATH:bin+path.delimiter+process.env.PATH};
  for (const key of ['MAKEFLAGS','MFLAGS','MAKELEVEL','MAKEOVERRIDES','KANDEV_ROOT','KANDEV_SDK','KANDEV_BACKEND','GO_MOD_KANDEV_SDK','SETUP_KANDEV_REPO']) delete env[key];
  const setup = (...extra) => spawnSync('make',['--no-print-directory','setup',`KANDEV_REPOSITORY=${remote}`,`KANDEV_REVISION=${revision}`,...extra],{cwd:source,env,encoding:'utf8'});
  const failed = setup();
  assert.notEqual(failed.status,0);
  assert.match(failed.stderr,/simulated-fetch-outage/);
  const recovered = setup();
  assert.equal(recovered.status,0,recovered.stdout+recovered.stderr);
  assert.match(fs.readFileSync(log,'utf8'),/test .* -run \^\$ \.\/server\/\.\.\./);
  const managed = path.join(source,'.build/kandev');
  assert.equal(execFileSync(realGit,['-C',managed,'rev-parse','HEAD'],{encoding:'utf8'}).trim(),revision);
  const explicit = path.join(temp,'explicit');
  fs.mkdirSync(explicit);
  fs.writeFileSync(path.join(explicit,'keep'),'user checkout');
  assert.notEqual(setup(`KANDEV_ROOT=${explicit}`).status,0);
  assert.deepEqual(fs.readdirSync(explicit),['keep']);
  assert.equal(fs.readFileSync(path.join(explicit,'keep'),'utf8'),'user checkout');
 } finally { fs.rmSync(temp,{recursive:true,force:true}); }
});
