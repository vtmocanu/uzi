#!/usr/bin/env ruby
# Exercise the checked-in workflow, not a copy of its conditions. Stdlib only.
# The evaluator intentionally supports just this workflow's job-condition grammar;
# new syntax fails loudly and needs coverage rather than silently passing.
require 'yaml'
require 'open3'
require 'tempfile'
require 'tmpdir'
require 'fileutils'
require 'digest'
require 'time'
require 'json'

class Expression
  def initialize(source, context)
    @tokens = source.scan(/\s+|'[^']*'|&&|\|\||==|!=|[!()]|[\w.-]+/).reject { |t| t.strip.empty? }
    raise 'unsupported expression syntax' unless @tokens.join == source.gsub(/\s+/, '')
    @context = context
  end

  def value
    result = disjunction
    raise 'unconsumed expression tokens' unless @tokens.empty?
    result
  end

  def take(token)
    return false unless @tokens.first == token
    @tokens.shift
    true
  end

  def disjunction
    result = conjunction
    while take('||')
      right = conjunction
      result = result || right
    end
    result
  end

  def conjunction
    result = comparison
    while take('&&')
      right = comparison
      result = result && right
    end
    result
  end

  def comparison
    left = atom
    if ['==', '!='].include?(@tokens.first)
      operator = @tokens.shift
      right = atom
      equal = left.to_s.downcase == right.to_s.downcase
      return operator == '==' ? equal : !equal
    end
    left
  end

  def atom
    return !atom if take('!')
    if take('(')
      result = disjunction
      raise 'missing closing parenthesis' unless take(')')
      return result
    end
    token = @tokens.shift
    raise 'missing expression operand' unless token
    return token[1...-1] if token.start_with?("'")
    if ['always', 'cancelled'].include?(token)
      raise 'invalid status function' unless take('(') && take(')')
      return token == 'always' || @context.fetch('cancelled', false)
    end
    @context.fetch(token) { raise "unknown expression context: #{token}" }
  end
end

root = File.expand_path('..', __dir__)
traps_only = ARGV.delete('--release-traps-only')
workflow = YAML.safe_load(File.read(ARGV[0] || File.join(root, '.github/workflows/ci.yml')))
jobs = workflow.fetch('jobs')
checks = 0
assert = lambda do |actual, expected, label|
  raise "#{label}: expected #{expected.inspect}, got #{actual.inspect}" unless actual == expected
  checks += 1
end
components = %w[validate-api lint-api test-api test-api-store-it test-codex-refresh-lostreply-e2e validate-controller lint-controller test-controller validate-web test-web test-web-browser validate-agent test-agent-shard]
context = {
  'github.event_name' => 'pull_request', 'needs.changes.result' => 'success',
  'needs.changes.outputs.skip_components' => '1',
  'needs.changes.outputs.worker_uid' => 'false', 'needs.validate-agent.result' => 'success',
  'needs.test-agent-shard.result' => 'skipped', 'needs.test-agent-worker-uid.result' => 'skipped'
}
run_job = lambda do |job, overrides = {}|
  source = jobs.fetch(job)['if']
  current = context.merge(overrides)
  # Actions implicitly prepends success() unless a status function is present.
  # Failed/skipped needs must not accidentally suppress our full-run fallback.
  implicit_success = source && source.match(/\b(always|cancelled)\(/) ||
    Array(jobs.fetch(job)['needs']).all? { |dependency| current.fetch("needs.#{dependency}.result") == 'success' }
  implicit_success && (source ? Expression.new(source, current).value : true)
end

components.each do |job|
  assert.call(run_job.call(job), false, "safe PR skips #{job}")
  assert.call(Array(jobs.fetch(job)['needs']).include?('changes'), true, "#{job} waits for classification")
  %w[failure cancelled skipped].each do |result|
    assert.call(run_job.call(job, 'needs.changes.result' => result), true, "#{job} falls back after #{result}")
  end
  ['', '0', 'true', 'TRUE', '01', 'junk'].each do |output|
    assert.call(run_job.call(job, 'needs.changes.outputs.skip_components' => output), true, "#{job} rejects #{output.inspect}")
  end
  assert.call(run_job.call(job, 'github.event_name' => 'push', 'needs.changes.result' => 'skipped'), true, "main runs #{job}")
  assert.call(run_job.call(job, 'cancelled' => true), false, "supersession cancels #{job}")
end
assert.call(jobs.fetch('lint-repo').key?('needs'), false, 'repo gate remains independent')
assert.call(jobs.fetch('lint-repo').key?('if'), false, 'repo gate remains unconditional')
assert.call(workflow.fetch('on').keys.sort, %w[pull_request push], 'triggers unchanged')
assert.call(workflow.fetch('concurrency').fetch('cancel-in-progress'), true, 'supersession retained')
assert.call(run_job.call('test-agent-worker-uid'), false, 'safe PR skips worker lane')
%w[failure cancelled skipped].each do |result|
  assert.call(run_job.call('test-agent-worker-uid', 'needs.changes.result' => result), true, 'unknown classification runs worker lane')
end
assert.call(run_job.call('test-agent-worker-uid', 'needs.changes.outputs.skip_components' => '', 'needs.changes.outputs.worker_uid' => ''), true, 'missing worker output runs worker lane')
assert.call(run_job.call('test-agent-worker-uid', 'needs.validate-agent.result' => 'failure'), false, 'failed validation blocks worker lane')
assert.call(run_job.call('test-agent'), false, 'safe skipped dependencies skip aggregator')
aggregate = jobs.fetch('test-agent').fetch('steps').first.fetch('run')
%w[failure cancelled skipped].each do |result|
  ['test-agent-shard', 'test-agent-worker-uid'].each do |dependency|
    assert.call(run_job.call('test-agent', "needs.#{dependency}.result" => result, 'needs.changes.outputs.skip_components' => '0'), true, 'non-safe aggregator checks every terminal result')
    next if result == 'skipped'
    assert.call(run_job.call('test-agent', "needs.#{dependency}.result" => result), true, 'safe classification cannot conceal failure')
    env = {'SHARD_RESULT' => 'success', 'WORKER_RESULT' => 'success', 'CHANGES_RESULT' => 'success', 'WORKER_CHANGED' => 'true', 'CI_EVENT' => 'pull_request'}
    env[dependency == 'test-agent-shard' ? 'SHARD_RESULT' : 'WORKER_RESULT'] = result
    _, _, status = Open3.capture3(env, 'bash', '-e', '-c', aggregate)
    assert.call(status.success?, false, 'aggregator rejects failed/cancelled dependency')
  end
end

safe_step = jobs.fetch('changes').fetch('steps').find { |step| step['id'] == 'safe' }
raise 'missing safe classification step' unless safe_step
assert.call(jobs.fetch('changes').fetch('outputs').fetch('skip_components'), '${{ steps.safe.outputs.skip_components }}', 'classification output wiring')
assert.call(safe_step.fetch('env'), {'ALL_COUNT' => '${{ steps.filter.outputs.all_count }}', 'SAFE_COUNT' => '${{ steps.filter.outputs.component_safe_count }}', 'EXPECTED_COUNT' => '${{ github.event.pull_request.changed_files }}'}, 'count source wiring')
classify = lambda do |all, safe, expected|
  Tempfile.create('ci-safe-output') do |file|
    _, err, status = Open3.capture3({'ALL_COUNT' => all, 'SAFE_COUNT' => safe, 'EXPECTED_COUNT' => expected, 'GITHUB_OUTPUT' => file.path}, 'bash', '-e', '-c', safe_step.fetch('run'))
    raise err unless status.success?
    File.read(file.path).strip
  end
end
assert.call(classify.call('2', '2', '2'), 'skip_components=1', 'nonempty complete safe list')
[['2', '1', '2'], ['0', '0', '0'], ['', '', ''], ['x', 'x', 'x'], ['01', '01', '01'], ['2', '2', '3'], ['2', '2', ''], ['3000', '3000', '3001']].each do |counts|
  assert.call(classify.call(*counts), 'skip_components=0', "unsafe/malformed/truncated counts #{counts}")
end
filter_step = jobs.fetch('changes').fetch('steps').find { |step| step['id'] == 'filter' }
rules = YAML.safe_load(filter_step.fetch('with').fetch('filters'))
assert.call(rules.fetch('all'), ['**'], 'all changed paths counted')
assert.call(rules.fetch('component_safe'), [{'added|modified' => '.agents/skills/uzi-lander/**'}], 'only audited additions/modifications allowed')
flags = File::FNM_PATHNAME | File::FNM_DOTMATCH
allowed = rules.fetch('component_safe').first
statuses, pattern = allowed.first
fixtures = [
  [['modified', '.agents/skills/uzi-lander/SKILL.md']],
  [['added', '.agents/skills/uzi-lander/references/new.md']],
  [['modified', '.agents/skills/uzi-lander/SKILL.md'], ['added', 'fixtures/new.json']],
  [['deleted', '.agents/skills/uzi-lander/SKILL.md']],
  [['deleted', 'api/old.go'], ['added', '.agents/skills/uzi-lander/new.go']],
  [['deleted', '.agents/skills/uzi-lander/old.md'], ['added', 'api/new.go']],
  [['modified', '.agents/skills/uzi-watcher/scripts/backup-runs.sh']]
]
fixtures.each_with_index do |files, index|
  # Ruby's terminal ** needs /* to match nested files as picomatch's ** does.
  safe = files.count { |status, path| statuses.split('|').include?(status) && File.fnmatch(pattern.sub(/\/\*\*$/, '/**/*'), path, flags) }
  assert.call(classify.call(files.length.to_s, safe.to_s, files.length.to_s), index < 2 ? 'skip_components=1' : 'skip_components=0', "path/status fixture #{index}")
end
assert.call(jobs.fetch('test-api-store-it').fetch('steps').any? { |step| step.fetch('run', '').include?('../scripts/livedb-skip-guard.sh livedb.log') }, true, 'LiveDB proof retained')
brew_workflow = YAML.safe_load(File.read(File.join(root, '.github/workflows/brew.yml')))
assert.call(brew_workflow['concurrency'], nil, 'publication proof runs without the tap lock')
brew = brew_workflow.fetch('jobs')
publisher = brew.fetch('publish-formula')
assert.call(publisher.fetch('concurrency').fetch('group'), 'brew-tap-publish', 'tap writes remain serialized')
assert.call(publisher.fetch('concurrency')['queue'], 'max', 'pending publishes are retained')
assert.call(publisher.fetch('concurrency').fetch('cancel-in-progress'), false, 'active publication is never superseded')
assert.call(Array(publisher['needs']).include?('assert-release-published'), true, 'tap credential job requires successful release proof')
assert.call(publisher['if'], nil, 'publisher cannot override failed needs')
guard = brew.fetch('assert-release-published')
assert.call(guard['environment'], nil, 'release proof has no credential environment')
assert.call(guard.fetch('permissions'), {'contents' => 'read', 'actions' => 'read'}, 'release proof is read-only')
assert.call(guard.to_s.include?('HOMEBREW_TAP_TOKEN'), false, 'tap credential absent from proof job')
assert.call(publisher.fetch('environment'), 'release', 'tap environment stays tag-restricted')
proof_command = 'python3 -I scripts/release-ci-coverage.py published HEAD --tag "$INPUT_TAG" --wait 3600 --interval 15'
assert.call(guard.fetch('steps').any? { |step| step['run'] == proof_command }, true, 'exact publication proof command must run')
assert.call(guard['continue-on-error'], nil, 'proof job cannot suppress failure')
guard.fetch('steps').each { |step| assert.call(step['continue-on-error'], nil, 'proof steps cannot suppress failure') }
assert.call(3600 < guard.fetch('timeout-minutes') * 60, true, 'proof finishes within job timeout')
validation = guard.fetch('steps').find { |step| step['id'] == 'tag' }.fetch('run')
[['v0.2.0', 'v0.2.0', 'tag', true], ['v0.2.0-rc.1', 'v0.2.0-rc.1', 'tag', true],
 ['v0.2.0', 'v0.1.0', 'tag', false], ['v0.2.0', 'v0.2.0', 'branch', false],
 ['bad;tag', 'bad;tag', 'tag', false]].each do |tag, ref, kind, expected|
  Tempfile.create('brew-tag-output') do |file|
    _, _, status = Open3.capture3({'INPUT_TAG' => tag, 'WORKFLOW_TAG' => ref, 'WORKFLOW_REF_TYPE' => kind, 'GITHUB_OUTPUT' => file.path}, 'bash', '-e', '-c', validation)
    assert.call(status.success?, expected, "Brew tag context #{[tag, ref, kind]}")
    assert.call(File.read(file.path).strip, expected ? "tag=#{tag}" : '', 'only valid matching tag produces output')
  end
end
Dir.mktmpdir('brew-watch-proof') do |dir|
  gh = File.join(dir, 'gh')
  File.write(gh, <<~'SH')
    #!/bin/sh
    case "$*" in
      'run list --workflow release.yml '*) echo 101 ;;
      'run list --workflow brew.yml '*) echo 202 ;;
      'run view 101 '*) printf 'completed\tsuccess\tpublish-api\thttps://example.invalid/101\n' ;;
      'run view 202 '*) printf 'completed\tfailure\tassert-release-published\thttps://example.invalid/202\n' ;;
      *) echo 'unexpected gh call' >&2; exit 99 ;;
    esac
  SH
  File.chmod(0o700, gh)
  stdout, stderr, status = Open3.capture3({'PATH' => "#{dir}:#{ENV.fetch('PATH')}", 'GIT_CONFIG_COUNT' => '1',
                                        'GIT_CONFIG_KEY_0' => 'core.fsmonitor', 'GIT_CONFIG_VALUE_0' => 'false'},
                                       'bash', File.join(root, '.agents/skills/uzi-release/scripts/release-watch.sh'),
                                       '0.2.0', '--max-ticks', '1', chdir: root)
  assert.call(status.exitstatus, 1, 'failed Brew proof remains a gate failure')
  assert.call(stdout.include?('manual recovery required'), true, 'watcher refuses automatic gate rerun')
  assert.call(stderr.include?("After the same tag's release.yml succeeds: gh run rerun 202 --failed"), true, 'watcher names guarded recovery command')
end
release_auto = YAML.safe_load(File.read(File.join(root, '.github/workflows/release-auto.yml')), aliases: true)
assert.call(release_auto.fetch('on').keys.sort, %w[schedule workflow_dispatch], 'manual and nightly routes are explicit')
assert.call(release_auto.fetch('on').fetch('schedule'), [{'cron' => '47 4 * * *', 'timezone' => 'Europe/Bucharest'}], 'nightly uses local time after the sweep window')
assert.call(release_auto.fetch('permissions'), {}, 'automation token defaults to no permissions')
assert.call(release_auto.fetch('concurrency'), {'group' => 'release-auto', 'cancel-in-progress' => false, 'queue' => 'max'}, 'all release routes queue without cancellation')
auto_jobs = release_auto.fetch('jobs')
# Execute each actual failure-handler prologue in the shell contexts ERR inherits.
# Keep a fast slice for guard-removal probes without repeating Git fixture setup.
exercise_release_traps = lambda do
  trap_steps = {'bundle' => auto_jobs.fetch('push').fetch('steps').find { |step| step['id'] == 'bundle' },
                'push' => auto_jobs.fetch('push').fetch('steps').find { |step| step['id'] == 'push' },
                'watch' => auto_jobs.fetch('watch').fetch('steps').find { |step| step['id'] == 'watch' }}
  probe = lambda do |name, script, body|
    parts = script.split("trap fail ERR\n", 2)
    assert.call(parts.length, 2, "#{name}: actual ERR handler present")
    prologue = parts.first + "trap fail ERR\nreason='probe-failed'\n"
    Dir.mktmpdir('release-trap') do |dir|
      output = File.join(dir, 'output')
      File.write(output, '')
      environment = {'GITHUB_OUTPUT' => output, 'RELEASE_DISPATCHERS' => 'maintainer',
                     'RELEASE_SHA' => '1' * 40, 'PARENT' => '2' * 40, 'RELEASE_TAG' => 'v0.2.0-rc.1',
                     'BUNDLE_DIGEST' => '3' * 64}
      _, err, status = Open3.capture3(environment, 'bash', '-e', '-c', prologue + body, chdir: dir)
      assert.call(status.exitstatus, 1, "#{name}: producer failure exits 1: #{err}")
      assert.call(File.readlines(output).map(&:strip), ['reason=probe-failed'], "#{name}: exactly one parent reason write")
    end
  end
  trap_steps.each do |name, step|
    script = step.fetch('run')
    {'function' => 'g(){ false; }; g', 'substitution' => 'g(){ false; }; observed=$(g)',
     'pipeline' => 'g(){ false; }; g | cat'}.each do |context, body|
      probe.call("#{name}/#{context}", script, body)
    end
    assert.call(script.lines[1].strip, 'failure_shell=$BASHPID', "#{name}: capture parent PID before any subshell")
  end
  # Extract production consumers plus their preceding checked assignment, when
  # present. A producer emits the expected value before failing, so testing the
  # value alone cannot detect its failure.
  consumers = [
    ['bundle', 'read -r -a dispatchers <<<', 'dispatcher_words', 'tr(){ printf maintainer; false; };'],
    ['bundle', '"$BUNDLE_DIGEST" ]', 'actual_bundle_digest', 'bundle=fixture; sha256sum(){ printf "%s  fixture\n" "$BUNDLE_DIGEST"; false; };'],
    ['bundle', '= 2 ]', 'head_count', 'heads=fixture; wc(){ printf "2\n"; false; };'],
    ['bundle', '"$RELEASE_SHA" ]', 'tag_sha', 'g(){ printf "%s\n" "$RELEASE_SHA"; false; };'],
    ['bundle', '= tag ]', 'tag_type', 'g(){ printf "tag\n"; false; };'],
    ['bundle', '"$RELEASE_SHA $PARENT" ]', 'parent_line', 'g(){ printf "%s %s\n" "$RELEASE_SHA" "$PARENT"; false; };'],
    ['watch', '"$RELEASE_SHA" ]', 'tag_sha', 'git(){ printf "%s\n" "$RELEASE_SHA"; false; };'],
    ['watch', 'echo "published_at=', 'published_at', 'date(){ printf "2026-10-27T09:00:00Z\n"; false; };'],
    ['watch', 'echo "verified_at=', 'verified_at', 'date(){ printf "2026-10-27T09:00:00Z\n"; false; };']
  ]
  consumers.each do |step_name, anchor, variable, stub|
    script = trap_steps.fetch(step_name).fetch('run')
    lines = script.lines
    index = lines.index { |line| line.include?(anchor) }
    raise "missing production consumer: #{anchor}" unless index
    first = index > 0 && lines[index - 1].lstrip.start_with?(variable + '=') ? index - 1 : index
    probe.call("#{step_name}/#{variable}", script, stub + "\n" + lines[first..index].join)
  end
end
if traps_only
  exercise_release_traps.call
  puts "ci-safe-paths: #{checks} assertions passed (release traps only)"
  exit
end
%w[authorize prepare push watch].each do |name|
  values = auto_jobs.fetch(name).fetch('env')
  assert.call(values.fetch('RELEASE_KIND'), "${{ github.event_name == 'schedule' && 'rc' || inputs.kind }}", "#{name}: scheduled kind cannot become stable")
  assert.call(values.fetch('RELEASE_ABANDON'), "${{ github.event_name == 'schedule' && 'false' || inputs.abandon_train }}", "#{name}: nightly never abandons the train")
end
%w[authorize prepare push].each do |name|
  assert.call(auto_jobs.fetch(name).fetch('env').fetch('RELEASE_NIGHTLY_ENABLED'), '${{ vars.RELEASE_NIGHTLY_ENABLED }}', "#{name}: nightly requires the operator flag")
end
[auto_jobs.fetch('prepare').fetch('env'), auto_jobs.fetch('push').fetch('env'),
 auto_jobs.fetch('report').fetch('steps').first.fetch('env')].each do |values|
  assert.call(values['RUN_CREATED_AT'], '${{ needs.authorize.outputs.created_at }}', 'nightly window uses original API metadata, not preparation output')
end
upload = auto_jobs.fetch('prepare').fetch('steps').find { |step| step.fetch('uses', '').start_with?('actions/upload-artifact@') }
assert.call(upload['if'], "steps.prepare.outputs.outcome == 'prepared'", 'a no-op cannot upload a nonexistent bundle')
%w[no-op prepared].each do |outcome|
  assert.call(Expression.new(upload.fetch('if'), {'steps.prepare.outputs.outcome' => outcome}).value,
              outcome == 'prepared', "artifact guard evaluates #{outcome}")
end
assert.call(release_auto.fetch('on').fetch('workflow_dispatch').fetch('inputs').fetch('kind').fetch('options'), %w[rc stable], 'dispatch operation choices')
assert.call(auto_jobs.keys, %w[authorize prepare push watch report], 'release automation job graph')
permissions = {'authorize' => {'contents' => 'read'}, 'prepare' => {'contents' => 'read', 'actions' => 'write'},
               'push' => {}, 'watch' => {'contents' => 'read', 'actions' => 'write'}, 'report' => {'issues' => 'write'}}
needs = {'authorize' => [], 'prepare' => ['authorize'], 'push' => %w[authorize prepare],
         'watch' => %w[authorize prepare push], 'report' => %w[authorize prepare push watch]}
auto_jobs.each do |name, job|
  assert.call(job.fetch('permissions'), permissions.fetch(name), "#{name} least privilege")
  assert.call(Array(job['needs']), needs.fetch(name), "#{name} retains authority prerequisites")
  assert.call(job['continue-on-error'], nil, "#{name} cannot suppress job failure")
  job.fetch('steps').each do |step|
    assert.call(step['continue-on-error'], nil, "#{name} cannot suppress a step failure")
    assert.call(step.fetch('run', '').include?('${{'), false, "#{name} run inputs arrive as data")
    if step['uses'] && !step['uses'].start_with?('./')
      assert.call(step['uses'].match?(/@[0-9a-f]{40}$/), true, "#{name} action is immutable")
    end
  end
  unless name == 'push'
    assert.call(job['environment'], nil, "#{name} has no credential environment")
    assert.call(job.to_s.include?('RELEASE_APP_KEY'), false, "#{name} has no App private key")
    assert.call(job.to_s.include?('RELEASE_APP_ID'), false, "#{name} has no App identity")
  end
end
assert.call(auto_jobs.fetch('report').fetch('if'), 'always()', 'failure reporting runs after skipped jobs')
auto_prepare = auto_jobs.fetch('prepare')
%w[stage planned_tag uncited uncited_count uncited_truncated].each do |field|
  assert.call(auto_prepare.fetch('outputs').fetch(field), "${{ steps.prepare.outputs.#{field} }}", "prepare forwards public diagnostic #{field}")
end
assert.call(auto_jobs.fetch('watch').fetch('outputs').fetch('reason'),
            '${{ steps.admit.outputs.reason || steps.watch.outputs.reason }}', 'watch forwards admission and receipt/publication failure reasons')
assert.call(auto_prepare.fetch('if'), "needs.authorize.outputs.outcome == 'authorized'", 'prepare requires current authorization')
assert.call(auto_jobs.fetch('authorize').fetch('steps').find { |step| step['id'] == 'authorize' }.fetch('run'),
            'python3 -I scripts/release-auto.py authorize', 'dispatch schema and context validator actually runs')
assert.call(auto_prepare.fetch('steps').find { |step| step['id'] == 'prepare' }.fetch('run'),
            'python3 -I scripts/release-auto.py prepare', 'shared release orchestrator actually runs')
auto_push = auto_jobs.fetch('push')
assert.call(auto_push.fetch('if'), "needs.authorize.outputs.outcome == 'authorized' && needs.prepare.outputs.outcome == 'prepared'", 'App key requires authorized successful preparation')
assert.call(auto_push.fetch('environment').fetch('name'), "${{ inputs.kind == 'stable' && 'release-stable' || 'release-rc' }}", 'separate principal per operation')
assert.call(auto_jobs.fetch('watch').fetch('if'), "${{ !cancelled() && needs.push.outputs.attempted == 'true' }}", 'known push attempt remains watched after job failure or timeout')
assert.call(auto_jobs.fetch('watch').fetch('steps').find { |step| step['id'] == 'admit' }.fetch('run'),
            'python3 -I scripts/release-auto.py watch-check', 'recovery checks actor and tag before actions writes')
mint = auto_push.fetch('steps').find { |step| step['id'] == 'app' }
assert.call(mint.fetch('uses'), 'actions/create-github-app-token@64ab729a6294000f5aa4a4bc0b73d1f32bc45510', 'reviewed token action')
assert.call(mint['if'], nil, 'mint keeps default success gating after validation')
assert.call(mint.fetch('with'), {'client-id' => '${{ vars.RELEASE_APP_ID }}', 'private-key' => '${{ secrets.RELEASE_APP_KEY }}',
                               'owner' => 'vtmocanu', 'repositories' => 'uzi', 'permission-contents' => 'write'}, 'installation token scope')
bundle_step = auto_push.fetch('steps').find { |step| step['id'] == 'bundle' }
publish_step = auto_push.fetch('steps').find { |step| step['id'] == 'push' }
assert.call(auto_push.fetch('outputs').fetch('attempted'), '${{ steps.push.outputs.attempted || steps.bundle.outputs.attempted }}',
            'actual push status overrides the pre-mint no-push fallback')
windows = [bundle_step, publish_step].map do |step|
  block = step.fetch('run').match(/^if \[ "\$GITHUB_EVENT_NAME" = schedule \]; then\n.*?^fi\n/m)
  assert.call(block.nil?, false, 'nightly window is checked before minting and before pushing')
  block[0]
end
assert.call(windows[0], windows[1], 'pre-mint and pre-push inline window policies stay identical')
# Isolate the actual comparison as a second defense, even if its numeric input
# guards were later removed. An operand error must select refusal, never push.
comparison = windows.last.match(/^  if (?:! )?\[ "\$now" -(?:lt|ge) "\$cutoff" \]; then\n.*?^  fi\n/m)
assert.call(comparison.nil?, false, 'actual nightly cutoff comparison is present')
[['5', ''], ['bad-now', '6'], ['5', 'bad-cutoff']].each do |now, cutoff|
  Dir.mktmpdir('nightly-compare') do |dir|
    output = File.join(dir, 'output')
    File.write(output, '')
    prefix = publish_step.fetch('run').split("trap fail ERR\n", 2).first + "trap fail ERR\n"
    body = "now=$PROBE_NOW\ncutoff=$PROBE_CUTOFF\n" + comparison[0]
    _, _, status = Open3.capture3({'GITHUB_OUTPUT' => output, 'PROBE_NOW' => now, 'PROBE_CUTOFF' => cutoff},
                                 'bash', '-e', '-c', prefix + body, chdir: dir)
    assert.call(status.exitstatus, 1, 'malformed cutoff comparison refuses')
    assert.call(File.readlines(output).map(&:strip), ['reason=missed-target'], 'comparison errors have one refusal reason')
  end
end
# Execute the classifier from the actual step, including its defensive mixed
# receipt branch. A reported ref update must never be called "nothing changed".
receipt_parts = publish_step.fetch('run').split("if awk -F '\\t' ", 2)
assert.call(receipt_parts.length, 2, 'actual porcelain classifier is present')
receipt_classifier = "awk -F '\\t' " + receipt_parts.last.split(' "$receipt"; then', 2).first
{"!\trefs/tags/v0.2.0:refs/tags/v0.2.0\t[rejected]\n" => true,
 "*\trefs/heads/main:refs/heads/main\t[new branch]\n!\trefs/tags/v0.2.0:refs/tags/v0.2.0\t[rejected]\n" => false,
 "fatal: transport interrupted\n" => false}.each do |receipt, rejected|
  Tempfile.create('push-porcelain') do |file|
    file.write(receipt)
    file.flush
    _, _, status = Open3.capture3('bash', '-e', '-c', receipt_classifier + ' "$1"', 'porcelain', file.path)
    assert.call(status.success?, rejected, 'only rejection without a changed ref is definitive')
  end
end
assert.call(auto_push.fetch('steps').index(bundle_step) < auto_push.fetch('steps').index(mint), true, 'bundle validated before minting')
assert.call(auto_push.fetch('steps').index(mint) < auto_push.fetch('steps').index(publish_step), true, 'only final publisher receives installation token')
assert.call(auto_push.fetch('steps').map { |step| step['uses'] }.compact, [
  'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333',
  'actions/create-github-app-token@64ab729a6294000f5aa4a4bc0b73d1f32bc45510'
], 'credential job runs only reviewed infrastructure actions')
push_runs = auto_push.fetch('steps').map { |step| step.fetch('run', '') }.join("\n")
assert.call(push_runs.match?(/\b(?:python3?|node|npm|npx|pip|curl|wget|eval|source|bash)\b|(?:^|[;|&]|\$\()\s*sh(?:\s|$)/), false, 'credential job does not execute repository runtimes or installers')
assert.call(push_runs.match?(%r{(?:^|[;|&])\s*(?:\./)?(?:\.agents/|scripts/)\S*|\b(?:git|g)\s+(?:checkout|switch|worktree)}), false, 'credential job never checks out or runs repository code')
assert.call(push_runs.include?('--force'), false, 'release push cannot force refs')

# Execute the real guarded steps against an owned local bare remote. All network
# protocols are denied; an insteadOf mapping redirects the hard-coded public URL.
real_git = ENV.fetch('PATH').split(File::PATH_SEPARATOR).map { |dir| File.join(dir, 'git') }.find { |path| File.executable?(path) }
Dir.mktmpdir('release-auto-contract') do |dir|
  remote = File.join(dir, 'remote.git')
  repo = File.join(dir, 'source')
  bin = File.join(dir, 'bin')
  FileUtils.mkdir_p(bin)
  hermetic = {'GIT_CONFIG_GLOBAL' => '/dev/null', 'GIT_CONFIG_SYSTEM' => '/dev/null', 'GIT_ALLOW_PROTOCOL' => 'file'}
  git = lambda do |*args|
    out, err, status = Open3.capture3(hermetic, real_git, '-c', 'core.fsmonitor=false', *args)
    raise "fixture git failed: #{err}" unless status.success?
    out.strip
  end
  git.call('init', '--bare', remote)
  git.call('init', '-b', 'main', repo)
  git.call('-C', repo, 'config', 'user.name', 'fixture')
  git.call('-C', repo, 'config', 'user.email', 't@example.com')
  File.write(File.join(repo, 'CHANGELOG.md'), "stable\n")
  git.call('-C', repo, 'add', 'CHANGELOG.md')
  git.call('-C', repo, 'commit', '-m', 'stable')
  stable_sha = git.call('-C', repo, 'rev-parse', 'HEAD')
  git.call('-C', repo, 'tag', '-a', 'v0.1.0', '-m', 'v0.1.0')
  File.write(File.join(repo, 'CHANGELOG.md'), "candidate\n")
  git.call('-C', repo, 'commit', '-am', 'candidate')
  parent = git.call('-C', repo, 'rev-parse', 'HEAD')
  git.call('-C', repo, 'tag', '-a', 'v0.2.0-rc.10', '-m', 'candidate')
  git.call('-C', repo, 'remote', 'add', 'origin', remote)
  git.call('-C', repo, 'push', 'origin', 'main', '--tags')
  advertised = git.call('-C', repo, 'ls-remote', 'origin', 'refs/heads/main', 'refs/tags/v*')
  refs_digest = Digest::SHA256.hexdigest(advertised.lines.map(&:chomp).select { |line| line.split("\t")[1].start_with?('refs/tags/v') }.sort.join("\n") + "\n")
  File.write(File.join(repo, 'CHANGELOG.md'), "new candidate\n")
  git.call('-C', repo, 'commit', '-am', 'prepared')
  release_sha = git.call('-C', repo, 'rev-parse', 'HEAD')
  git.call('-C', repo, 'tag', '-a', 'v0.2.0-rc.11', '-m', 'new candidate')
  tag_oid = git.call('-C', repo, 'rev-parse', 'v0.2.0-rc.11')
  git.call('-C', repo, 'update-ref', 'refs/heads/release-auto-prepared', release_sha)
  bundle = File.join(dir, 'release.bundle')
  git.call('-C', repo, 'bundle', 'create', bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v0.2.0-rc.11', '^' + parent)
  File.write(File.join(bin, 'git'), <<~'SH')
    #!/usr/bin/env bash
    set -e
    case "${TEST_GIT_FAIL:-}/$*" in
      bundle-verify/*' bundle verify '*|diff-tree/*' diff-tree '*)
        printf '%s\n' "$TEST_GIT_FAIL" > "$TEST_GIT_FAILURE_MARKER"; exit 71 ;;
      candidate-sha/*' rev-parse refs/tags/'*)
        printf '%s\n' "$TEST_GIT_FAIL" > "$TEST_GIT_FAILURE_MARKER"
        printf '%s\n' "$RELEASE_SHA"; exit 71 ;;
    esac
    args=(); pushing=false
    for arg in "$@"; do
      args+=("$arg")
      if [ "$arg" = push ]; then
        pushing=true
        if [ -n "${TEST_RECEIVE_PACK:-}" ]; then args+=("--receive-pack=$TEST_RECEIVE_PACK"); fi
      fi
    done
    if [ "$pushing" = true ] && [ "${TEST_FAIL_AFTER_PUSH:-0}" = 1 ]; then
      "$TEST_REAL_GIT" -c core.fsmonitor=false -c "url.${TEST_REMOTE}.insteadOf=https://github.com/vtmocanu/uzi.git" "${args[@]}"
      exit 77
    fi
    exec "$TEST_REAL_GIT" -c core.fsmonitor=false -c "url.${TEST_REMOTE}.insteadOf=https://github.com/vtmocanu/uzi.git" "${args[@]}"
  SH
  File.write(File.join(bin, 'receive-pack'), <<~'SH')
    #!/usr/bin/env bash
    export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
    unset GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0
    exec "$TEST_REAL_GIT" -c core.fsmonitor=false -c "core.hooksPath=$TEST_REMOTE/hooks" receive-pack "$@"
  SH
  File.write(File.join(bin, 'sha256sum'), <<~'RUBY')
    #!/usr/bin/env ruby
    require 'digest'
    data = ARGV.empty? ? STDIN.read : File.binread(ARGV[0])
    puts "#{Digest::SHA256.hexdigest(data)}  #{ARGV.empty? ? '-' : ARGV[0]}"
  RUBY
  File.write(File.join(bin, 'date'), <<~'RUBY')
    #!/usr/bin/env ruby
    require 'time'
    if ARGV.include?('-d')
      text = ARGV[ARGV.index('-d') + 1]
      if ARGV.include?('+%s') && !text.include?('T') && ENV['TEST_BAD_CUTOFF']
        puts(ENV['TEST_BAD_CUTOFF'] == 'empty' ? '' : 'bad-cutoff')
        exit
      end
      stamp = text.include?('T') ? Time.iso8601(text) : Time.strptime(text, '%Y-%m-%d %H:%M:%S')
      if ARGV.include?('+%z')
        puts ENV.fetch('TEST_OFFSET', stamp.getlocal.strftime('%z'))
      else
        puts(ARGV.include?('+%F') ? stamp.getlocal.strftime('%F') : stamp.to_i)
      end
    else
      if ARGV.include?('+%s') && ENV['TEST_BAD_NOW']
        puts(ENV['TEST_BAD_NOW'] == 'empty' ? '' : 'bad-now')
        exit
      end
      now = Time.at(Integer(ENV.fetch('TEST_NOW')))
      puts(ARGV.include?('+%F') ? now.getlocal.strftime('%F') : now.to_i)
    end
  RUBY
  %w[git receive-pack sha256sum date].each { |name| File.chmod(0o700, File.join(bin, name)) }
  env = hermetic.merge('PATH' => "#{bin}:#{ENV.fetch('PATH')}", 'TEST_REAL_GIT' => real_git, 'TEST_REMOTE' => remote,
                      'TEST_NOW' => Time.utc(2026, 10, 26, 10).to_i.to_s,
                      'GITHUB_REPOSITORY' => 'vtmocanu/uzi', 'GITHUB_EVENT_NAME' => 'workflow_dispatch', 'GITHUB_RUN_ATTEMPT' => '1',
                      'RELEASE_KIND' => 'rc', 'RELEASE_VERSION' => '', 'RELEASE_ABANDON' => 'false',
                      'RELEASE_SUPERSEDE' => '', 'RELEASE_RC_TAG' => '', 'RELEASE_RC_SHA' => '',
                      'RELEASE_REF' => 'refs/heads/main', 'RELEASE_ACTOR' => 'maintainer', 'RELEASE_DISPATCHERS' => 'maintainer',
                      'RUN_CREATED_AT' => '2026-10-26T09:00:00Z', 'PARENT' => parent, 'RELEASE_SHA' => release_sha,
                      'RELEASE_TAG' => 'v0.2.0-rc.11', 'TAG_OID' => tag_oid, 'BUNDLE_DIGEST' => Digest::SHA256.file(bundle).hexdigest,
                      'REFS_DIGEST' => refs_digest, 'PREVIOUS_RC' => 'v0.2.0-rc.10', 'PREVIOUS_RC_SHA' => parent,
                      'PREVIOUS_STABLE' => 'v0.1.0', 'PREVIOUS_STABLE_SHA' => stable_sha, 'GH_APP_TOKEN' => 'credential-sentinel')
  exercise = lambda do |changes, expected, publish_expected = nil, source_bundle = bundle, after_validation = {}, expected_outcome = nil, expected_reason = nil|
    Dir.mktmpdir('push-step', dir) do |work|
      FileUtils.mkdir_p(File.join(work, 'release-bundle'))
      FileUtils.cp(source_bundle, File.join(work, 'release-bundle/release.bundle'))
      invocation = env.merge('RUNNER_TEMP' => work, 'GITHUB_OUTPUT' => File.join(work, 'output'),
                             'TEST_GIT_FAILURE_MARKER' => File.join(work, 'failed-git')).merge(changes)
      guard_out, err, status = Open3.capture3(invocation, 'bash', '-e', '-c', bundle_step.fetch('run'), chdir: work)
      assert.call(status.success?, expected, "actual pre-key guard #{changes.keys}: #{err[-300..]}")
      if expected_reason
        if changes.key?('TEST_GIT_FAIL')
          assert.call(File.read(invocation.fetch('TEST_GIT_FAILURE_MARKER')).strip, changes.fetch('TEST_GIT_FAIL'), 'exact Git failure was injected')
        end
        outputs = File.exist?(invocation.fetch('GITHUB_OUTPUT')) ? File.readlines(invocation.fetch('GITHUB_OUTPUT')).map(&:strip) : []
        assert.call(outputs.select { |line| line.start_with?('reason=') }, ['reason=' + expected_reason], 'one phase-correct reason before mint/push')
        assert.call(outputs.include?('attempted=true'), false, 'pre-mint refusal cannot have attempted a push')
      end
      if changes['GITHUB_RUN_ATTEMPT'] == '2'
        assert.call(status.success?, false, 'rerun never reaches the default-success token step')
        assert.call((guard_out + err).include?('Re-dispatch instead'), true, 'push rerun gives a clear recovery instruction')
      end
      if expected && !publish_expected.nil?
        active_remote = invocation.fetch('TEST_REMOTE')
        main_before = git.call('--git-dir=' + active_remote, 'rev-parse', 'refs/heads/main')
        tags_before = git.call('--git-dir=' + active_remote, 'show-ref', '--tags')
        _, push_err, pushed = Open3.capture3(invocation.merge(after_validation), 'bash', '-e', '-c', publish_step.fetch('run'), chdir: work)
        expected_main = publish_expected && invocation['RELEASE_KIND'] == 'rc' ? invocation['RELEASE_SHA'] : main_before
        assert.call(git.call('--git-dir=' + active_remote, 'rev-parse', 'refs/heads/main'), expected_main,
                    expected_outcome == 'push-rejected' ? 'atomic rejection leaves main unchanged' : 'publisher preserves the correct main boundary')
        assert.call(pushed.success?, publish_expected, "actual atomic publish: #{push_err}")
        if expected_outcome
          receipt = File.read(invocation.fetch('GITHUB_OUTPUT')).lines.map { |line| line.strip.split('=', 2) }.to_h
          if %w[missed-target invalid-nightly-window].include?(expected_outcome)
            reasons = File.readlines(invocation.fetch('GITHUB_OUTPUT')).map(&:strip).select { |line| line.start_with?('reason=') }
            assert.call(reasons, ['reason=' + expected_outcome], 'nightly cutoff has exactly one typed reason')
            assert.call(receipt['attempted'], 'false', 'nightly cutoff skips the push and watch')
          else
            assert.call(receipt['outcome'], expected_outcome, 'push result classification')
            assert.call(receipt['attempted'], expected_outcome == 'push-rejected' ? 'false' : 'true', 'rejected push never enters watch')
          end
        end
        if publish_expected
          assert.call(git.call('--git-dir=' + active_remote, 'rev-parse', 'refs/tags/' + invocation['RELEASE_TAG'] + '^{}'), invocation['RELEASE_SHA'], 'atomic publish creates exact tag')
        else
          assert.call(git.call('--git-dir=' + active_remote, 'show-ref', '--tags'), tags_before, 'failed final guard preserves all tags')
        end
      end
    end
  end
  {'bundle-verify' => 'invalid-prepared-bundle', 'candidate-sha' => 'invalid-prepared-bundle',
   'diff-tree' => 'non-metadata-bundle'}.each do |fault, reason|
    exercise.call({'TEST_GIT_FAIL' => fault}, false, nil, bundle, {}, nil, reason)
  end
  # Validate a normal bundle first, then execute the actual final publisher as a
  # scheduled route delayed by its runner queue. Its clock guard is independent.
  [['empty-cutoff', {'TEST_BAD_CUTOFF' => 'empty'}], ['garbage-cutoff', {'TEST_BAD_CUTOFF' => 'garbage'}],
   ['empty-now', {'TEST_BAD_NOW' => 'empty'}], ['garbage-now', {'TEST_BAD_NOW' => 'garbage'}],
   ['wrong-offset', {'TEST_OFFSET' => '+0000'}]].each do |label, faults|
    faulty_remote = File.join(dir, 'faulty-date-' + label + '.git')
    git.call('clone', '--bare', remote, faulty_remote)
    exercise.call({'TEST_REMOTE' => faulty_remote, 'RUN_CREATED_AT' => '2026-10-27T02:47:00Z'}, true, false, bundle,
                  {'GITHUB_EVENT_NAME' => 'schedule', 'TEST_NOW' => Time.utc(2026, 10, 27, 3).to_i.to_s}.merge(faults), 'invalid-nightly-window')
  end
  [['winter-at', '2026-10-27T02:47:00Z', '2026-10-27T04:00:00Z', false],
   ['winter-before', '2026-10-27T02:47:00Z', '2026-10-27T03:59:59Z', true],
   ['winter-after', '2026-10-27T02:47:00Z', '2026-10-27T04:01:00Z', false],
   ['queued-next-day', '2026-10-27T02:47:00Z', '2026-10-28T02:00:00Z', false],
   ['summer-at', '2026-07-15T01:47:00Z', '2026-07-15T03:00:00Z', false],
   ['summer-before', '2026-07-15T01:47:00Z', '2026-07-15T02:59:59Z', true],
   ['spring-at', '2026-03-29T01:47:00Z', '2026-03-29T03:00:00Z', false],
   ['autumn-at', '2026-10-25T02:47:00Z', '2026-10-25T04:00:00Z', false],
   ['future-run', '2026-10-27T02:47:00Z', '2026-10-27T02:40:00Z', false, 'invalid-nightly-window'],
   ['noncanonical-time', '2026-10-27 02:47:00+00:00', '2026-10-27T03:00:00Z', false, 'invalid-nightly-window']].each do |label, created, now, allowed, reason|
    scheduled_remote = File.join(dir, 'scheduled-' + label + '.git')
    git.call('clone', '--bare', remote, scheduled_remote)
    exercise.call({'TEST_REMOTE' => scheduled_remote, 'RUN_CREATED_AT' => created}, true, allowed, bundle,
                  {'GITHUB_EVENT_NAME' => 'schedule', 'TEST_NOW' => Time.iso8601(now).to_i.to_s}, allowed ? nil : reason || 'missed-target')
  end
  schedule_env = {'GITHUB_EVENT_NAME' => 'schedule', 'RELEASE_NIGHTLY_ENABLED' => 'true',
                  'RUN_CREATED_AT' => '2026-10-27T02:47:00Z', 'TEST_NOW' => Time.utc(2026, 10, 27, 3).to_i.to_s}
  exercise.call(schedule_env.merge('TEST_NOW' => Time.utc(2026, 10, 27, 4).to_i.to_s), false, nil, bundle, {}, nil, 'missed-target')
  [{'TEST_BAD_CUTOFF' => 'empty'}, {'TEST_BAD_CUTOFF' => 'garbage'}, {'TEST_BAD_NOW' => 'empty'},
   {'TEST_BAD_NOW' => 'garbage'}, {'TEST_OFFSET' => '+0000'}].each do |faults|
    exercise.call(schedule_env.merge(faults), false, nil, bundle, {}, nil, 'invalid-nightly-window')
  end
  [59, 60].each do |skew|
    exercise.call(schedule_env.merge('TEST_NOW' => (Time.iso8601(schedule_env.fetch('RUN_CREATED_AT')).to_i - skew).to_s), true)
  end
  exercise.call(schedule_env.merge('TEST_NOW' => (Time.iso8601(schedule_env.fetch('RUN_CREATED_AT')).to_i - 61).to_s),
                false, nil, bundle, {}, nil, 'invalid-nightly-window')
  ['', 'false', 'TRUE', '1'].each do |disabled|
    exercise.call(schedule_env.merge('RELEASE_NIGHTLY_ENABLED' => disabled), false)
  end
  exercise.call(schedule_env.merge('RELEASE_ACTOR' => 'scheduler', 'RELEASE_DISPATCHERS' => ''), true)
  exercise.call(schedule_env.merge('RELEASE_VERSION' => '0.2.0'), false)
  exercise.call(schedule_env.merge('RELEASE_KIND' => 'stable', 'RELEASE_RC_TAG' => 'v0.2.0-rc.10', 'RELEASE_RC_SHA' => parent), false)
  exercise.call({'RELEASE_ACTOR' => 'outside'}, false)
  exercise.call({'GITHUB_RUN_ATTEMPT' => '2'}, false)
  exercise.call({'RELEASE_REF' => 'refs/heads/feature'}, false)
  exercise.call({'GITHUB_REPOSITORY' => 'other/uzi'}, false)
  exercise.call({'GITHUB_EVENT_NAME' => 'push'}, false)
  exercise.call({'RELEASE_VERSION' => '0.3.0'}, false)
  exercise.call({'RELEASE_RC_TAG' => 'v0.2.0-rc.10'}, false)
  exercise.call({'RELEASE_TAG' => 'v00.2.0-rc.11'}, false)
  exercise.call({'BUNDLE_DIGEST' => '0' * 64}, false)
  exercise.call({'RELEASE_SHA' => '0' * 40}, false)
  exercise.call({'REFS_DIGEST' => '0' * 64}, true, false)
  exercise.call({'PREVIOUS_RC' => 'v0.2.0-rc.9'}, true, false)
  exercise.call({'PREVIOUS_STABLE' => 'v0.0.9'}, true, false)
  exercise.call({'RELEASE_KIND' => 'stable', 'RELEASE_RC_TAG' => 'v0.2.0-rc.10', 'RELEASE_RC_SHA' => parent,
                 'RELEASE_TAG' => 'v0.2.0', 'RUN_CREATED_AT' => '2026-10-24T09:00:00Z'}, false)
  # Matching digests and advertised heads cannot replace annotation and target
  # checks. Both malformed bundles otherwise have the expected two heads.
  git.call('-C', repo, 'update-ref', 'refs/tags/v0.2.0-rc.11', release_sha)
  lightweight_bundle = File.join(dir, 'lightweight.bundle')
  git.call('-C', repo, 'bundle', 'create', lightweight_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v0.2.0-rc.11', '^' + parent)
  exercise.call({'TAG_OID' => release_sha, 'BUNDLE_DIGEST' => Digest::SHA256.file(lightweight_bundle).hexdigest}, false, nil, lightweight_bundle)
  git.call('-C', repo, 'tag', '-f', '-a', 'v0.2.0-rc.11', '-m', 'wrong target', parent)
  wrong_target_oid = git.call('-C', repo, 'rev-parse', 'v0.2.0-rc.11')
  wrong_target_bundle = File.join(dir, 'wrong-target.bundle')
  git.call('-C', repo, 'bundle', 'create', wrong_target_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v0.2.0-rc.11', '^' + parent)
  exercise.call({'TAG_OID' => wrong_target_oid, 'BUNDLE_DIGEST' => Digest::SHA256.file(wrong_target_bundle).hexdigest}, false, nil, wrong_target_bundle)
  git.call('-C', repo, 'update-ref', 'refs/tags/v0.2.0-rc.11', tag_oid)
  invalid_rc_bundle = File.join(dir, 'invalid-rc.bundle')
  git.call('-C', repo, 'tag', '-a', 'v0.2.0-rc.0', '-m', 'invalid zero candidate', release_sha)
  invalid_rc_oid = git.call('-C', repo, 'rev-parse', 'v0.2.0-rc.0')
  git.call('-C', repo, 'bundle', 'create', invalid_rc_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v0.2.0-rc.0', '^' + parent)
  exercise.call({'RELEASE_TAG' => 'v0.2.0-rc.0', 'TAG_OID' => invalid_rc_oid,
                 'BUNDLE_DIGEST' => Digest::SHA256.file(invalid_rc_bundle).hexdigest}, false, nil, invalid_rc_bundle)
  valid_extra_bundle = File.join(dir, 'valid-extra-head.bundle')
  git.call('-C', repo, 'update-ref', 'refs/heads/unexpected', release_sha)
  git.call('-C', repo, 'bundle', 'create', valid_extra_bundle, 'refs/heads/release-auto-prepared',
           'refs/tags/v0.2.0-rc.11', 'refs/heads/unexpected', '^' + parent)
  exercise.call({'BUNDLE_DIGEST' => Digest::SHA256.file(valid_extra_bundle).hexdigest}, false, nil, valid_extra_bundle)
  # Each malformed commit otherwise has matching parent, tag and digest. Pin
  # both the fragment-only deletion rule and unchanged regular-file modes.
  %w[deletion mode].each_with_index do |shape, index|
    git.call('-C', repo, 'checkout', '--detach', parent)
    if shape == 'deletion'
      FileUtils.rm(File.join(repo, 'CHANGELOG.md'))
    else
      File.chmod(0o755, File.join(repo, 'CHANGELOG.md'))
    end
    git.call('-C', repo, 'add', '--', 'CHANGELOG.md')
    git.call('-C', repo, 'commit', '-m', shape + ' fence fixture')
    malformed = git.call('-C', repo, 'rev-parse', 'HEAD')
    malformed_tag = "v0.2.0-rc.#{96 + index}"
    git.call('-C', repo, 'tag', '-a', malformed_tag, '-m', shape)
    malformed_oid = git.call('-C', repo, 'rev-parse', malformed_tag)
    git.call('-C', repo, 'update-ref', 'refs/heads/release-auto-prepared', malformed)
    malformed_bundle = File.join(dir, shape + '.bundle')
    git.call('-C', repo, 'bundle', 'create', malformed_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/' + malformed_tag, '^' + parent)
    exercise.call({'RELEASE_SHA' => malformed, 'RELEASE_TAG' => malformed_tag, 'TAG_OID' => malformed_oid,
                   'BUNDLE_DIGEST' => Digest::SHA256.file(malformed_bundle).hexdigest}, false, nil, malformed_bundle)
  end
  # A self-consistent child with source changes must fail the fixed metadata fence.
  git.call('-C', repo, 'checkout', '--detach', parent)
  FileUtils.mkdir_p(File.join(repo, 'api'))
  File.write(File.join(repo, 'api/injected.go'), "package main\n")
  git.call('-C', repo, 'add', 'api/injected.go')
  git.call('-C', repo, 'commit', '-m', 'extra source')
  injected = git.call('-C', repo, 'rev-parse', 'HEAD')
  git.call('-C', repo, 'tag', '-a', 'v0.2.0-rc.99', '-m', 'extra source')
  injected_tag = git.call('-C', repo, 'rev-parse', 'v0.2.0-rc.99')
  git.call('-C', repo, 'update-ref', 'refs/heads/release-auto-prepared', injected)
  injected_bundle = File.join(dir, 'injected.bundle')
  git.call('-C', repo, 'bundle', 'create', injected_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v0.2.0-rc.99', '^' + parent)
  exercise.call({'RELEASE_SHA' => injected, 'RELEASE_TAG' => 'v0.2.0-rc.99', 'TAG_OID' => injected_tag,
                 'BUNDLE_DIGEST' => Digest::SHA256.file(injected_bundle).hexdigest}, false, nil, injected_bundle)
  # Stable pushes ignore current main, and age is checked again after bundle fetch.
  stable_remote = File.join(dir, 'stable-remote.git')
  git.call('clone', '--bare', remote, stable_remote)
  git.call('-C', repo, 'push', stable_remote, release_sha + ':refs/heads/main')
  git.call('-C', repo, 'checkout', '--detach', parent)
  File.write(File.join(repo, 'CHANGELOG.md'), "promoted\n")
  git.call('-C', repo, 'commit', '-am', 'promote')
  promoted = git.call('-C', repo, 'rev-parse', 'HEAD')
  git.call('-C', repo, 'tag', '-a', 'v0.2.0', '-m', 'stable')
  promoted_tag = git.call('-C', repo, 'rev-parse', 'v0.2.0')
  git.call('-C', repo, 'update-ref', 'refs/heads/release-auto-prepared', promoted)
  stable_bundle = File.join(dir, 'stable.bundle')
  git.call('-C', repo, 'bundle', 'create', stable_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v0.2.0', '^' + parent)
  stable_env = {'TEST_REMOTE' => stable_remote, 'RELEASE_KIND' => 'stable', 'RELEASE_RC_TAG' => 'v0.2.0-rc.10',
                'RELEASE_RC_SHA' => parent, 'RELEASE_TAG' => 'v0.2.0', 'RELEASE_SHA' => promoted,
                'TAG_OID' => promoted_tag, 'BUNDLE_DIGEST' => Digest::SHA256.file(stable_bundle).hexdigest,
                'RUN_CREATED_AT' => '2026-10-25T10:00:00Z'}
  prefix = bundle_step.fetch('run').split("reason='bundle-digest-mismatch'", 2)
  assert.call(prefix.length, 2, 'schema boundary remains identifiable before artifact access')
  _, _, parent_status = Open3.capture3(env.merge(stable_env).merge('PARENT' => stable_sha, 'GITHUB_OUTPUT' => File.join(dir, 'prefix-output')),
                                      'bash', '-e', '-c', prefix.first)
  assert.call(parent_status.success?, false, 'actual pre-key schema independently binds approved parent')
  exercise.call(stable_env.merge('RELEASE_VERSION' => '0.2.0'), false, nil, stable_bundle)
  exercise.call(stable_env.merge('RELEASE_SUPERSEDE' => 'v0.2.0-rc.9'), false, nil, stable_bundle)
  exercise.call(stable_env.merge('RUN_CREATED_AT' => '2026-10-27T10:00:00Z'), false, nil, stable_bundle)
  # Recompute discovery outputs so only the approved-candidate identity guard
  # rejects a newer remote RC. The raw inventory digest alone is insufficient.
  newer_remote = File.join(dir, 'newer-candidate.git')
  git.call('clone', '--bare', remote, newer_remote)
  git.call('-C', repo, 'tag', '-a', 'v0.2.0-rc.12', '-m', 'new candidate', parent)
  git.call('-C', repo, 'push', newer_remote, 'refs/tags/v0.2.0-rc.12')
  newer_refs = git.call('ls-remote', newer_remote, 'refs/tags/v*')
  newer_digest = Digest::SHA256.hexdigest(newer_refs.lines.map(&:chomp).sort.join("\n") + "\n")
  exercise.call(stable_env.merge('TEST_REMOTE' => newer_remote, 'REFS_DIGEST' => newer_digest,
                               'PREVIOUS_RC' => 'v0.2.0-rc.12'), true, false, stable_bundle)
  git.call('-C', repo, 'tag', '-d', 'v0.2.0-rc.12')
  exercise.call(stable_env, true, false, stable_bundle, {'TEST_NOW' => (Time.utc(2026, 10, 26, 10).to_i + 1).to_s})
  exercise.call(stable_env, true, true, stable_bundle)
  # An existing target is independently refused even with a matching fresh
  # inventory and matching discovery names. No existing ref may be rewritten.
  existing_remote = File.join(dir, 'existing-target.git')
  git.call('clone', '--bare', remote, existing_remote)
  git.call('-C', repo, 'push', existing_remote, 'refs/tags/v0.2.0-rc.11')
  existing_refs = git.call('ls-remote', existing_remote, 'refs/tags/v*')
  existing_digest = Digest::SHA256.hexdigest(existing_refs.lines.map(&:chomp).sort.join("\n") + "\n")
  exercise.call({'TEST_REMOTE' => existing_remote, 'REFS_DIGEST' => existing_digest,
                 'PREVIOUS_RC' => 'v0.2.0-rc.11', 'PREVIOUS_RC_SHA' => release_sha}, true, false)
  # A self-contained bundle can hide a wrong parent from prerequisite checking;
  # the independent single-parent binding must still reject it before minting.
  File.write(File.join(repo, 'CHANGELOG.md'), "wrong parent\n")
  git.call('-C', repo, 'commit', '-am', 'another child')
  wrong_child = git.call('-C', repo, 'rev-parse', 'HEAD')
  git.call('-C', repo, 'tag', '-a', 'v0.2.0-rc.12', '-m', 'wrong parent fixture')
  wrong_tag = git.call('-C', repo, 'rev-parse', 'v0.2.0-rc.12')
  git.call('-C', repo, 'update-ref', 'refs/heads/release-auto-prepared', wrong_child)
  wrong_bundle = File.join(dir, 'wrong-parent.bundle')
  git.call('-C', repo, 'bundle', 'create', wrong_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v0.2.0-rc.12')
  exercise.call({'RELEASE_SHA' => wrong_child, 'RELEASE_TAG' => 'v0.2.0-rc.12', 'TAG_OID' => wrong_tag,
                 'BUNDLE_DIGEST' => Digest::SHA256.file(wrong_bundle).hexdigest}, false, nil, wrong_bundle)
  extra_bundle = File.join(dir, 'extra-ref.bundle')
  git.call('-C', repo, 'update-ref', 'refs/heads/extra', wrong_child)
  git.call('-C', repo, 'bundle', 'create', extra_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v0.2.0-rc.12', 'refs/heads/extra')
  exercise.call({'RELEASE_SHA' => wrong_child, 'RELEASE_TAG' => 'v0.2.0-rc.12', 'TAG_OID' => wrong_tag,
                 'BUNDLE_DIGEST' => Digest::SHA256.file(extra_bundle).hexdigest}, false, nil, extra_bundle)
  # Self-consistency does not establish the human-approved stable version.
  mismatched_remote = File.join(dir, 'mismatched-stable.git')
  git.call('clone', '--bare', remote, mismatched_remote)
  git.call('-C', repo, 'tag', '-a', 'v9.9.9', '-m', 'wrong stable target', release_sha)
  mismatched_oid = git.call('-C', repo, 'rev-parse', 'v9.9.9')
  git.call('-C', repo, 'update-ref', 'refs/heads/release-auto-prepared', release_sha)
  mismatched_bundle = File.join(dir, 'mismatched-stable.bundle')
  git.call('-C', repo, 'bundle', 'create', mismatched_bundle, 'refs/heads/release-auto-prepared', 'refs/tags/v9.9.9', '^' + parent)
  exercise.call({'TEST_REMOTE' => mismatched_remote, 'RELEASE_KIND' => 'stable',
                 'RELEASE_RC_TAG' => 'v0.2.0-rc.10', 'RELEASE_RC_SHA' => parent,
                 'RELEASE_TAG' => 'v9.9.9', 'TAG_OID' => mismatched_oid,
                 'BUNDLE_DIGEST' => Digest::SHA256.file(mismatched_bundle).hexdigest}, false, nil, mismatched_bundle)
  # A main advance after preparation must stop the RC route before any push.
  raced_remote = File.join(dir, 'raced-remote.git')
  git.call('clone', '--bare', remote, raced_remote)
  git.call('-C', repo, 'push', raced_remote, release_sha + ':refs/heads/main')
  exercise.call({'TEST_REMOTE' => raced_remote}, true, false)
  rejected_remote = File.join(dir, 'rejected-remote.git')
  git.call('clone', '--bare', remote, rejected_remote)
  File.write(File.join(rejected_remote, 'hooks/update'), "#!/bin/sh\n[ \"$1\" != refs/tags/v0.2.0-rc.11 ]\n")
  File.chmod(0o700, File.join(rejected_remote, 'hooks/update'))
  exercise.call({'TEST_REMOTE' => rejected_remote, 'TEST_RECEIVE_PACK' => File.join(bin, 'receive-pack')}, true, false, bundle, {}, 'push-rejected')
  lost_remote = File.join(dir, 'lost-receipt.git')
  git.call('clone', '--bare', remote, lost_remote)
  exercise.call({'TEST_REMOTE' => lost_remote, 'TEST_FAIL_AFTER_PUSH' => '1'}, true, true, bundle, {}, 'receipt-unknown')
  exercise.call({}, true, true)
end
# Execute the actual watch step with offline stand-ins. Receipt failure must be
# owned by watch before reporting can become a successful delivery job (#3551).
watch_script = auto_jobs.fetch('watch').fetch('steps').find { |step| step['id'] == 'watch' }.fetch('run')
Dir.mktmpdir('release-watch-receipt') do |dir|
  bin = File.join(dir, 'bin')
  release_scripts = File.join(dir, '.agents/skills/uzi-release/scripts')
  FileUtils.mkdir_p([bin, release_scripts])
  File.write(File.join(bin, 'git'), <<~'SH')
    #!/bin/sh
    case "$1" in
      rev-parse) printf '%s\n' "$RELEASE_SHA" ;;
      fetch) exit 0 ;;
      merge-base) exit "$WATCH_RECEIPT_RC" ;;
      *) exit 99 ;;
    esac
  SH
  File.write(File.join(bin, 'python3'), "#!/bin/sh\n[ \"$*\" = '-I scripts/release-auto.py watch-log' ] || exit 99\n")
  File.write(File.join(bin, 'date'), "#!/bin/sh\nprintf '2026-10-27T09:00:00Z\\n'\n")
  File.write(File.join(release_scripts, 'release-watch.sh'), "#!/bin/sh\nprintf 'watch\\n' >> \"$WATCH_CALLS\"\n")
  File.write(File.join(release_scripts, 'release-verify.sh'), "#!/bin/sh\nprintf 'verify\\n' >> \"$WATCH_CALLS\"\nexit \"$VERIFY_RC\"\n")
  Dir.glob(File.join(bin, '*')).concat(Dir.glob(File.join(release_scripts, '*'))).each { |path| File.chmod(0o700, path) }
  environment = {'PATH' => "#{bin}:#{ENV.fetch('PATH')}", 'RELEASE_SHA' => '1' * 40,
                 'RELEASE_TAG' => 'v0.2.0-rc.1', 'RELEASE_KIND' => 'rc', 'RUNNER_TEMP' => dir,
                 'GITHUB_OUTPUT' => File.join(dir, 'outputs'), 'WATCH_CALLS' => File.join(dir, 'calls')}
  [[1, 0, false], [0, 1, false], [0, 0, true]].each do |receipt_rc, verify_rc, expected|
    FileUtils.rm_f(environment.fetch('WATCH_CALLS'))
    File.write(environment.fetch('GITHUB_OUTPUT'), '')
    _, err, status = Open3.capture3(environment.merge('WATCH_RECEIPT_RC' => receipt_rc.to_s, 'VERIFY_RC' => verify_rc.to_s),
                                   'bash', '-e', '-c', watch_script, chdir: dir)
    assert.call(status.success?, expected, "watch receipt=#{receipt_rc}, verify=#{verify_rc}: #{err}")
    output = File.read(environment.fetch('GITHUB_OUTPUT'))
    if receipt_rc != 0
      assert.call(File.exist?(environment.fetch('WATCH_CALLS')), false, 'unconfirmed receipt stops before publication recovery')
      assert.call(output.include?('reason=push-receipt-unconfirmed'), true, 'watch owns the receipt failure reason')
    elsif verify_rc != 0
      assert.call(output.include?('reason=publication-unconfirmed'), true, 'watch owns failed publication verification')
    else
      assert.call(output.include?('outcome=published-and-verified'), true, 'confirmed publication remains successful')
    end
  end
  File.write(environment.fetch('GITHUB_OUTPUT'), '')
  _, err, status = Open3.capture3(environment.merge('GITHUB_EVENT_NAME' => 'schedule', 'RELEASE_NIGHTLY_ENABLED' => 'false',
                                  'RUN_CREATED_AT' => '2026-10-26T02:47:00Z', 'WATCH_RECEIPT_RC' => '0', 'VERIFY_RC' => '0'),
                                'bash', '-e', '-c', watch_script, chdir: dir)
  assert.call(status.success?, true, "tagged nightly recovery continues after cutoff and pause: #{err}")
  assert.call(File.read(environment.fetch('GITHUB_OUTPUT')).include?('outcome=published-and-verified'), true, 'late recovery retains verified success')
end
# Reporting has no checkout/contents grant. Execute its exact inline program with
# a fake API; healthy publication is summary-only, and failures reuse one issue.
report_script = auto_jobs.fetch('report').fetch('steps').first.fetch('run')
Dir.mktmpdir('release-report') do |dir|
  bin = File.join(dir, 'bin')
  FileUtils.mkdir_p(bin)
  File.write(File.join(bin, 'gh'), <<~'RUBY')
    #!/usr/bin/env ruby
    require 'json'
    abort 'fixture reporting failure' if ENV['REPORT_FAIL'] == '1'
    File.open(ENV.fetch('REPORT_CALLS'), 'a') { |file| file.puts(ARGV.join(' ')) }
    input = ARGV.include?('--input') ? JSON.parse(STDIN.read) : nil
    if ARGV.include?('--slurp')
      puts [[{'number' => 42, 'state' => 'closed', 'title' => 'Release automation failures'}]].to_json
    elsif ARGV.include?('PATCH')
      abort 'wrong reopen' unless input == {'state' => 'open'}
      puts({'number' => 42, 'state' => 'open'}.to_json)
    elsif ARGV.any? { |arg| arg.end_with?('/comments') }
      abort 'unsafe issue body' if input.fetch('body').include?('credential-sentinel')
      File.write(ENV.fetch('REPORT_BODY'), input.fetch('body'))
      puts({'id' => 1}.to_json)
    else
      abort 'unexpected issue creation or API request'
    end
  RUBY
  File.chmod(0o700, File.join(bin, 'gh'))
  normal = {'authorize' => {'result' => 'success', 'outputs' => {'outcome' => 'authorized'}},
            'prepare' => {'result' => 'success', 'outputs' => {'outcome' => 'prepared', 'reason' => 'ready'}},
            'push' => {'result' => 'success', 'outputs' => {'outcome' => 'pushed', 'attempted' => 'true'}},
            'watch' => {'result' => 'success', 'outputs' => {'outcome' => 'published-and-verified', 'receipt' => 'confirmed'}}}
  environment = {'PATH' => "#{bin}:#{ENV.fetch('PATH')}", 'REPORT_CALLS' => File.join(dir, 'calls'),
                 'GH_TOKEN' => 'credential-sentinel', 'REPOSITORY' => 'vtmocanu/uzi', 'RUN_ID' => '123',
                 'RUN_URL' => 'https://github.com/vtmocanu/uzi/actions/runs/123', 'GITHUB_STEP_SUMMARY' => File.join(dir, 'summary')}
  environment['REPORT_BODY'] = File.join(dir, 'body')
  _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => normal.to_json), 'bash', '-e', '-c', report_script)
  assert.call(status.success?, true, "healthy publication reporting: #{err}")
  assert.call(File.exist?(File.join(dir, 'calls')), false, 'healthy publication never opens or comments on an issue')
  [['authorize', 'nightly-disabled'], ['prepare', 'no-new-shipping-change'], ['prepare', 'no-releasable-notes']].each do |phase, reason|
    noop = Marshal.load(Marshal.dump(normal))
    noop[phase] = {'result' => 'success', 'outputs' => {'outcome' => 'no-op', 'reason' => reason}}
    %w[prepare push watch].each do |name|
      next if name == phase
      noop[name] = {'result' => 'skipped', 'outputs' => {}}
    end
    FileUtils.rm_f(environment.fetch('REPORT_CALLS'))
    File.write(environment.fetch('GITHUB_STEP_SUMMARY'), '')
    _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => noop.to_json, 'GITHUB_EVENT_NAME' => 'schedule'), 'bash', '-e', '-c', report_script)
    assert.call(status.success?, true, "#{reason}: no-op reporting succeeds: #{err}")
    text = File.read(environment.fetch('GITHUB_STEP_SUMMARY'))
    assert.call(text.include?('"outcome": "no-op"'), true, "#{reason}: no-op is explicit")
    assert.call(text.include?('"reason": "' + reason + '"'), true, "#{reason}: planned reason survives skipped jobs")
    assert.call(text.include?('"phase": "' + phase + '"'), true, "#{reason}: phase is truthful")
    assert.call(text.include?('Nightly RC'), true, "#{reason}: human-readable nightly result")
    assert.call(File.exist?(environment.fetch('REPORT_CALLS')), false, "#{reason}: expected no-op never files an incident")
  end
  [['2026-10-27T02:47:00Z', '2026-10-27T05:00:00Z', false],
   ['2026-10-27T02:47:00Z', '2026-10-27T05:00:01Z', true],
   ['2026-07-15T01:47:00Z', '2026-07-15T04:00:00Z', false],
   ['2026-07-15T01:47:00Z', '2026-07-15T04:00:01Z', true],
   ['2026-10-26T22:30:00Z', '2026-10-27T04:59:59Z', false]].each do |created, verified, late|
    completed = Marshal.load(Marshal.dump(normal))
    completed['watch']['outputs'].merge!('verified_at' => verified, 'published_at' => verified)
    File.write(environment.fetch('GITHUB_STEP_SUMMARY'), '')
    FileUtils.rm_f(environment.fetch('REPORT_CALLS'))
    stdout, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => completed.to_json, 'GITHUB_EVENT_NAME' => 'schedule',
                                        'RUN_CREATED_AT' => created), 'bash', '-e', '-c', report_script)
    assert.call(status.success?, true, "late=#{late}: verification report succeeds: #{err}")
    text = File.read(environment.fetch('GITHUB_STEP_SUMMARY'))
    assert.call(text.include?('"outcome": "published-and-verified"'), true, 'late success is still verified')
    assert.call(text.include?('"late_finish": ' + late.to_json), true, 'late finish uses the original local-date target')
    assert.call(stdout.include?('::warning::'), late, 'only late verified completion produces a warning')
    assert.call(File.exist?(environment.fetch('REPORT_CALLS')), false, 'late success does not file an extra incident')
  end
  ['credential-sentinel', '2026-10-27 05:00:00+00:00'].each do |invalid|
    malformed_time = Marshal.load(Marshal.dump(normal))
    malformed_time['watch']['outputs']['verified_at'] = invalid
    FileUtils.rm_f(environment.fetch('REPORT_CALLS'))
    _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => malformed_time.to_json, 'GITHUB_EVENT_NAME' => 'schedule',
                                    'RUN_CREATED_AT' => '2026-10-27T02:47:00Z'), 'bash', '-e', '-c', report_script)
    assert.call(status.success?, false, 'malformed nightly timing metadata is refused')
    assert.call(err.include?('invalid nightly timing metadata'), true, 'timing refusal is a controlled diagnostic')
    assert.call(err.include?('credential-sentinel'), false, 'timing validation does not echo untrusted input')
    assert.call(File.exist?(environment.fetch('REPORT_CALLS')), false, 'malformed timestamps never reach the issue API')
  end
  manual_complete = Marshal.load(Marshal.dump(normal))
  manual_complete['watch']['outputs']['verified_at'] = '2026-10-27T16:00:00Z'
  FileUtils.rm_f(environment.fetch('REPORT_CALLS'))
  File.write(environment.fetch('GITHUB_STEP_SUMMARY'), '')
  stdout, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => manual_complete.to_json, 'GITHUB_EVENT_NAME' => 'workflow_dispatch',
                                      'RUN_CREATED_AT' => '2026-10-27T02:47:00Z'), 'bash', '-e', '-c', report_script)
  assert.call(status.success?, true, "manual late-day verification has no morning target: #{err}")
  assert.call(File.read(environment.fetch('GITHUB_STEP_SUMMARY')).include?('"late_finish": null'), true, 'manual verification has no late flag')
  assert.call(stdout.include?('::warning::'), false, 'manual completion after 07:00 does not warn')
  assert.call(File.exist?(environment.fetch('REPORT_CALLS')), false, 'manual verified completion is not an incident')
  missed = Marshal.load(Marshal.dump(normal))
  missed['push'] = {'result' => 'failure', 'outputs' => {'reason' => 'missed-target', 'attempted' => 'false'}}
  missed['watch'] = {'result' => 'skipped', 'outputs' => {}}
  _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => missed.to_json, 'GITHUB_EVENT_NAME' => 'schedule'), 'bash', '-e', '-c', report_script)
  assert.call(status.success?, true, "missed-target incident delivery succeeds: #{err}")
  missed_body = File.read(environment.fetch('REPORT_BODY'))
  assert.call(missed_body.include?('pre-push cutoff'), true, 'missed-target explains the clock refusal')
  assert.call(missed_body.include?('"push_attempted": false'), true, 'missed target proves the push was skipped')
  failed = Marshal.load(Marshal.dump(normal))
  failed['prepare'] = {'result' => 'failure', 'outputs' => {'outcome' => 'blocked', 'reason' => 'codex-runtime-needs-local-cut'}}
  _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => failed.to_json), 'bash', '-e', '-c', report_script)
  assert.call(status.success?, true, "successful incident delivery does not create a second failure: #{err}")
  calls = File.read(File.join(dir, 'calls'))
  assert.call(calls.include?('PATCH repos/vtmocanu/uzi/issues/42'), true, 'closed tracking issue is reopened')
  assert.call(calls.include?('POST repos/vtmocanu/uzi/issues/42/comments'), true, 'existing tracking issue receives incident')
  rejected = Marshal.load(Marshal.dump(normal))
  rejected['push'] = {'result' => 'failure', 'outputs' => {'outcome' => 'push-rejected', 'attempted' => 'false', 'reason' => 'push-rejected'}}
  rejected['watch'] = {'result' => 'skipped', 'outputs' => {}}
  _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => rejected.to_json), 'bash', '-e', '-c', report_script)
  assert.call(status.success?, true, "definitive rejection is delivered: #{err}")
  rejection_body = File.read(environment.fetch('REPORT_BODY'))
  assert.call(rejection_body.include?('"push_attempted": true'), true, 'a rejected Git push was attempted even though watch is skipped')
  assert.call(rejection_body.include?('No push was attempted.'), false, 'recovery marker is not mistaken for literal push attempt status')
  unconfirmed = Marshal.load(Marshal.dump(normal))
  unconfirmed['watch'] = {'result' => 'failure', 'outputs' => {'receipt' => 'unconfirmed', 'reason' => 'push-receipt-unconfirmed'}}
  _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => unconfirmed.to_json), 'bash', '-e', '-c', report_script)
  assert.call(status.success?, true, "unconfirmed receipt incident is delivered: #{err}")
  receipt_body = File.read(environment.fetch('REPORT_BODY'))
  assert.call(receipt_body.include?('could not confirm that main contains the RC'), true, 'receipt failure has an actionable explanation')
  assert.call(receipt_body.include?('"phase": "watch"'), true, 'receipt uncertainty is owned by watch')
  %w[failure cancelled].each do |result|
    interrupted = Marshal.load(Marshal.dump(normal))
    interrupted['push'] = {'result' => result, 'outputs' => {'attempted' => 'true'}}
    interrupted['watch'] = {'result' => 'failure', 'outputs' => {}}
    _, _, status = Open3.capture3(environment.merge('NEEDS_JSON' => interrupted.to_json), 'bash', '-e', '-c', report_script)
    assert.call(status.success?, true, "#{result} after send is delivered successfully")
    text = File.read(environment.fetch('GITHUB_STEP_SUMMARY'))
    assert.call(text.include?('"push_outcome": "receipt-unknown"'), true, "#{result} with missing receipt is unknown")
    assert.call(text.include?('"outcome": "partial-publication"'), true, "#{result} reports same-tag manual inspection")
  end
  failed['prepare']['outputs'].merge!('reason' => 'changelog-coverage-refused', 'phase' => 'prepare',
                                    'stage' => 'changelog-coverage', 'planned_tag' => 'v0.2.0-rc.11',
                                    'uncited' => [{sha: '1' * 40, pr: 3486}].to_json,
                                    'uncited_count' => '1', 'uncited_truncated' => 'false')
  failed['push'] = {'result' => 'skipped', 'outputs' => {}}
  failed['watch'] = {'result' => 'skipped', 'outputs' => {}}
  _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => failed.to_json), 'bash', '-e', '-c', report_script)
  assert.call(status.success?, true, "oracle refusal is delivered: #{err}")
  body = File.read(environment.fetch('REPORT_BODY'))
  assert.call(body.include?('3486'), true, 'incident includes the uncited PR identity')
  assert.call(body.include?('"planned_tag": "v0.2.0-rc.11"'), true, 'incident identifies the planned candidate')
  assert.call(body.include?('"push_attempted": false'), true, 'prepare refusal explicitly proves push was skipped')
  assert.call(body.include?('"changes": null'), true, 'uncollected changes are not described as an empty shipping set')
  _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => failed.to_json, 'REPORT_FAIL' => '1'), 'bash', '-e', '-c', report_script)
  assert.call(status.success?, false, 'a reporting API failure keeps the report job red')
  assert.call(err.include?('fixture reporting failure'), true, 'the failure actually came from the reporting API')
  # Reporting deliberately has no checkout. Pin its duplicate wire validator to
  # the codec with the same fixtures, including valid boundary values.
  codec_program = <<~'PY'
    import importlib.util, json, sys
    spec = importlib.util.spec_from_file_location('diagnostics', sys.argv[1])
    codec = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(codec)
    try:
        codec.validate(json.loads(sys.argv[2]))
    except (TypeError, ValueError):
        print('false')
    else:
        print('true')
  PY
  row = {'sha' => '1' * 40, 'pr' => 3486}
  rows50 = Array.new(50) { row }
  identity_cases = [
    ['one identity', [row], 1, false, true],
    ['SHA alone', [row.merge('pr' => nil)], 1, false, true],
    ['largest PR', [row.merge('pr' => 2**53 - 1)], 1, false, true],
    ['50 identities', rows50, 50, false, true],
    ['truncated identities', rows50, 51, true, true],
    ['empty collection', [], 0, false, true],
    ['uncollected', nil, nil, false, true],
    ['private SHA', [row.merge('sha' => 'credential-sentinel')], 1, false, false],
    ['uppercase SHA', [row.merge('sha' => 'A' * 40)], 1, false, false],
    ['short SHA', [row.merge('sha' => '1' * 39)], 1, false, false],
    ['boolean PR', [row.merge('pr' => true)], 1, false, false],
    ['zero PR', [row.merge('pr' => 0)], 1, false, false],
    ['negative PR', [row.merge('pr' => -1)], 1, false, false],
    ['oversized PR', [row.merge('pr' => 2**53)], 1, false, false],
    ['string PR', [row.merge('pr' => '3486')], 1, false, false],
    ['extra identity field', [row.merge('prose' => 'unexpected')], 1, false, false],
    ['51 identities', Array.new(51) { row }, 51, false, false],
    ['boolean count', [row], true, false, false],
    ['count below collection', [row], 0, false, false],
    ['oversized count', [row], 2**53, true, false],
    ['missing truncation flag', [row], 2, false, false],
    ['false truncation claim', [row], 1, true, false],
    ['uncollected count', nil, 1, false, false],
    ['uncollected truncation', nil, nil, true, false]
  ]
  identity_cases.each do |label, rows, count, truncated, expected|
    diagnostic = {'schema' => 1, 'stage' => 'changelog-coverage', 'reason' => 'changelog-coverage-refused',
                  'planned_tag' => 'v0.2.0-rc.11', 'uncited' => rows,
                  'uncited_count' => count, 'uncited_truncated' => truncated}
    stdout, err, codec_status = Open3.capture3('python3', '-I', '-',
                                             File.join(root, 'scripts/lib/release-diagnostics.py'),
                                             diagnostic.to_json, stdin_data: codec_program)
    assert.call(codec_status.success?, true, "#{label}: codec instrument succeeds: #{err}")
    codec_accepts = JSON.parse(stdout)
    incident = Marshal.load(Marshal.dump(failed))
    %w[uncited uncited_count uncited_truncated].each do |field|
      incident['prepare']['outputs'][field] = diagnostic.fetch(field).to_json
    end
    FileUtils.rm_f(environment.fetch('REPORT_CALLS'))
    _, err, status = Open3.capture3(environment.merge('NEEDS_JSON' => incident.to_json), 'bash', '-e', '-c', report_script)
    assert.call(status.success?, codec_accepts, "#{label}: report/codec identity parity")
    assert.call(codec_accepts, expected, "#{label}: codec identity verdict")
    assert.call(status.success?, expected, "#{label}: report identity verdict: #{err}")
    assert.call(File.exist?(environment.fetch('REPORT_CALLS')), expected, "#{label}: only valid identities reach the issue API")
  end
end
exercise_release_traps.call
puts "ci-safe-paths: #{checks} assertions passed"
