This project will be a AI dark factory.
we have 3 sources of inspiration, we can implement what we want, but we also look at these projects for inpiration, if possible we do better than them.

1. <https://github.com/vdaubry/bottega>
2. <https://github.com/multica-ai/multica>
3. <https://github.com/vfarcic/dot-agent-deck>
4. <https://github.com/coder/coder> - for agents/VM to see how they provision

lets clone all 3 as submodules, before implementing something we will look at them if they implemented same thing or something similar. We will always choose a better version then them if available! We want best practice level of implementation. Some stuff we can deffer for later.

> *(2026-08-03: the submodules were removed. The three projects were vendored under `inspiration/` from the start until then, and are now ordinary clones outside the repo, symlinked back into a gitignored `inspiration/` by `./scripts/link-inspiration.sh`. The look-at-them-first rule above still stands — see the Inspiration-first entries in `CLAUDE.md` and `.claude/agent-team.md`, including why a recursive grep no longer sees them.)*

for agents lets see how coder is able to do docker and kind in k8s pods - maybe we replicate or steal ideeas, look also at the k8s/coder gitlab repo

the initial MVP will be local laptop demo, docker-compose.
it will have a psql DB si persistent storage

First thing on our list is to create a simple webui with user suport and registration, no mail for now.
User/pass stored in DB.

We should be able to see gitlab issues where our bot has rights.
We should see all our repos and be able to choose between them in the ui.
For each repo we should have a kanban board similar to multica/bottega - probably based on gitlab labels?
Because of the gitlab labels the kanban board will be kept in sync between uzi and gitlab, see <https://gitlab.example.com/myorg/example-app/-/boards> for a gitlab kanban example ( we can have other tags if needed, this is just an example) take a look at this one also pls: <https://kan.bn/kan/roadmap>

The arhitecture will be server/client, the client will be based on anthopic-sdk? see how bottega does it?
Each user will have its own client, first it will be a container/pod then probably VM
Each user will have to create its own Gitlab UZI bot user - create a procedure with glab ? / script?
Each user must add its bot to the gitlab projects he/she wishes to.
We should display only issues with PRD label and also sanity check that they include a link to the PRD, as our agents will work on those PRDs.
Each users agent will need an anthropic oauth token - do a doc on how this can be obtained. Each user can store it's secret in the webui and we store it in DB. We should be able to encrypt it in DB.

Connection server/agent should be encrypted.
Admin users should be able to see all agents status, if they are connected to server or not. Each user can see its own agent if it is connected or not.

We should be able to define/edit agents from UI, see our existing agent-team setup for how we create/update agents pls.
Agents sit with code, but we have some agent temapltes that will sit in DB, and we can edit them via UI.

In the UI i want to see when an issue is in progress (team is working on it) - which agents are live, which are idle, which will be spawned next.
Also i want to see the messages, what athey currently doing and correct them if needed.

agents should use worktrees so they can work on same codebase in parallel

for now we are using claude/anthopic agents - but prepare for the future when we can switch to a different model/provider - we should also do a PoC with openai - as we have a key for openai

we should be able to define skills global skills and also each user should be able to define skill for his aagent, and he should be able to associate/allocate global or user skills to each agent (pod/vm)

always verify that our agents only have the rights to create MR, nothing else in write mode (except code ofc), they should not be able to modify resources, code on main directly etc! this is a primary directive!

for encryption - maybe/can we store/encrypt each users/agents secrets with the password of its user? so nobody can decode them?
a real trade-off worth discussing before it lands in a PRD (agents can't decrypt secrets while you're logged out — breaks background agents). - to be discussed

can uzi verify the glpat does not have more permissions than needed for a MR? for each repo? when we save it? and afterwards?

adaptive/responsive width so it works on mobile and larger screens

integration with gitlab to check and display CI status, if it is broken spin up an agent to review what happened and if it can fix it - if the code was bad => uzi verifies it's work

we should allow registration lony from @example.com email addresses - configurable
can we use agent teams? in sdk? - if not lets see how multica/bottega do it (regardless we should keep a reviwer step/stage)

have a docs section on uzi  with relevant howtos (example how to create an agent/bot/skill/etc how to do gitlab bots/give permissions, etc, include screenshots, ask me for screenshots)

user should be able to choose model for each agent defined including for lead

scan for command not found and report them somewhere so we can see what tools we are missing

## later stuff

- loop/hang detection - if something is taking too long flag it ( in ui and on slack ) or if it seems stuck for too long
- self improvement - we should have a general token for AI and that is used for a configurable (2day default) scheduled job, that can be enabled from admin settings, every run should spin up a team to identify an improvement (euther bug or new feature) and create a MR for it, if a selfimprovement already exists then it should reuse the existing one so everything is tested togheeter
- cli to interact with running sessions, see them + chat with them
- maybe have a label in glab and agents auto start working on issues? how would it work? by using the agent from the user which created the label?
- dark/light theme with autodetect
- have AI agent - u can chat with UZI - it can see it's code and create issues for itself (local webui agent uses each user aouth token)
- we should be able to create a PRD with the AI agent from web so it should create an issue/prd we should be able to spin up agents to review prd before submission - all from webui
- gitlab integration - with later forgejo support
- enable/disable registration for users
- SSO with KC
- switch to VMs?
- make UZI create the bots for users in gitlab? make absolutly sure they only have correct roles (developer?) - they must only be able to create MRs nothing else
- ios client
- slack notifications - if an agent needs human input / when issues transition from one state to other.
- have a catalog for agents (each user builds/defines its own agent with the tools needed by him/she - one might need node tools, other might need java tools, etc)
- agents should be able to injects secrets - to connect to other resources, like prometheus for example, maybe a kubeconfig/etc, those secrets should sit encrypted in db
- remove vlad seed user from env + remove all seeds
- CICD + deploy to k8s + regen vmocanu bot token!! and create a dedicated one for tests!!!
- check what other functionalities does multica/bottega have than we dont have and we might steal from them?
- report on token used per issues - in webui and in PRD file and gitlab issue
- debug mode with verbose logs
- lanfuse integration
- for CICD code generation uzi should plug to qdrant KB? how do we say this? we tell agents? skills better? myabe only skills?
- have a way to analize sessions (like a LLM judge) - that checks if tools/permissions are missing and does some recommandations (maybe have a message inbox - both for user and admins, admins should see all notifications)
- agents/skills maybe should sit in dedicated git repos? and be cloned to uzi? u should be able to add one or more repos? maybe users can bring their own repos?

## later - later

- leaderboard :) tokens/model used
- how can we work on codebases for which we dont have access - transfer repo from laptop to agent? - see also if this can help: <https://github.com/openclaw/crabbox>
