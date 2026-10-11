#!/usr/bin/env ruby
# Exercise the checked-in workflow, not a copy of its conditions. Stdlib only.
# The evaluator intentionally supports just this workflow's job-condition grammar;
# new syntax fails loudly and needs coverage rather than silently passing.
require 'yaml'
require 'open3'
require 'tempfile'

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
puts "ci-safe-paths: #{checks} assertions passed"
