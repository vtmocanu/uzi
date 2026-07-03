This project will be a AI dark factory.
we have 3 sources of inspiration, we can implement what we want, but we also look at these projects for inpiration, if possible we do better than them.

1. <https://github.com/vdaubry/bottega>
2. <https://github.com/multica-ai/multica>
3. <https://github.com/vfarcic/dot-agent-deck>

lets clone all 3 as submodules, before implementing something we will look at them if they implemented same thing or something similar. We will always choose a better version then them if available! We want best practice level of implementation. Some stuff we can deffer for later.

the initial MVP will be local laptop demo, docker-compose.
it will have a psql DB si persistent storage

First thing on our list is to create a simple webui with user suport and registration, no mail for now.
User/pass stored in DB.

We should be able to see gitlab issues where our bot has rights.
We should see all our repos and be able to choose between them in the ui.
For each repo we should have a kanban board similar to multica/bottega - probably based on gitlab labels?
Because of the gitlab labels the kanban board will be kept in sync between uzi and gitlab, see <https://gitlab.example.com/vtmocanu/example-app/-/boards> for a gitlab kanban example ( we can have other tags if needed, this is just an example) take a look at this one also pls: <https://kan.bn/kan/roadmap>

The arhitecture will be server/client, the client will be based on anthopic-sdk? see how bottega does it?
Each user will have its own client, first it will be a container/pod then probably VM
Each user will have to create its own Gitlab UZI bot user - create a procedure with glab ? / script?
Each user must add its bot to the gitlab projects he/she wishes to.
We should display only issues with PRD label and also sanity check that they include a link to the PRD, as our agents will work on those PRDs.
Each users agent will need an anthropic oauth token - do a doc on how this can be obtained. Each user can store it's secret in the webui and we store it in DB. We should be able to encrypt it in DB.

Connection server/agent should be encrypted.
Admin users should be able to see all agents status, if they are connected to server or not. Each user can see its own agent if it is connected or not.

We should be able to define/edit agents from UI, see /dot-ai-agent-team for how we create/update agents pls.
Agents sit with code, but we have some agent temapltes that will sit in DB, and we can edit them via UI.

In the UI i want to see when an issue is in progress (team is working on it) - which agents are live, which are idle, which will be spawned next.
Also i want to see the messages, what athey currently doing and correct them if needed.

agents should use worktrees so they can work on same codebase in parallel

for now we are using claude/anthopic agents - but prepare for the future when we can switch to a different model/provider - we should also do a PoC with openai - as we have a key for openai

we should be able to define skills global skills and also each user should be able to define skill for his aagent, and he should be able to associate/allocate global or user skills to each agent (pod/vm)

always verify that our agents only have the rights to create MR, nothing else in write mode (except code ofc), they should not be able to modify resources, code on main directly etc! this is a primary directive!

for encryption - maybe/can we store/encrypt each users/agents secrets with the password of its user? so nobody can decode them?

## later stuff

- dark/light theme with autodetect
- have AI agent - u can chat with UZI - it can see it's code and create issues for itself (local webui agent uses each user aouth token)
- gitlab integration - with later forgejo support
- enable/disable registration for users
- SSO with KC
- switch to VMs?
- make UZI create the bots for users in gitlab? make absolutly sure they only have correct roles (developer?) - they must only be able to create MRs nothing else
- ios client
- slack notifications - if an agent needs human input / when issues transition from one state to other.
- have a catalog for agents (each user builds/defines its own agent with the tools needed by him/she - one might need node tools, other might need java tools, etc)
- agents should be able to injects secrets - to connect to other resources, like prometheus for example, maybe a kubeconfig/etc, those secrets should sit encrypted in db
- remove vlad seed user from env
- CICD + deploy to k8s
