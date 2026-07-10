// kubehoard finds namespaces hoarding CPU and memory: resource requests far
// above actual usage ("hoarders") as well as pods dangerously close to
// their limits (OOMKill/throttling risk).
//
// Subcommands:
//
//	kubehoard [scan]        scan + report (table/html/json)
//	kubehoard suggest       generate a patch plan with request recommendations
//	kubehoard apply PLAN    apply the plan to the owner workloads
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/aleks/kubehoard/internal/collector"
	"github.com/aleks/kubehoard/internal/model"
	"github.com/aleks/kubehoard/internal/plan"
	"github.com/aleks/kubehoard/internal/report"
)

func main() {
	args := os.Args[1:]
	cmd := "scan"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	var err error
	switch cmd {
	case "scan":
		err = runScan(args)
	case "suggest":
		err = runSuggest(args)
	case "apply":
		err = runApply(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "kubehoard: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, errFindings) {
			fmt.Fprintf(os.Stderr, "kubehoard: %v\n", err)
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "kubehoard: error: %v\n", err)
		os.Exit(1)
	}
}

// errFindings signals "scan ok, but findings above the --fail-on threshold"
// and results in exit code 3 (1 = error, 2 = flag errors via ExitOnError).
var errFindings = errors.New("findings")

func usage() {
	fmt.Fprint(os.Stderr, `kubehoard – resource overprovisioning scanner for Kubernetes/OpenShift

Commands:
  scan     (default) scan namespaces and print a report
  suggest  generate a patch plan (YAML) with request recommendations for hoarding containers
  apply    apply a plan: patch the owner workloads (triggers a rolling restart)

Per-command flags: kubehoard <command> -h
`)
}

// scanFlags are the flags shared by scan and suggest.
type scanFlags struct {
	kubeconfig    *string
	namespaces    *string
	labelSelector *string
	nsRegex       *string
	excludeRegex  *string
	warnFactor    *float64
	critFactor    *float64
	capFactor     *float64
	limitWarnPct  *float64
	timeout       *time.Duration
}

func registerScanFlags(fs *flag.FlagSet) scanFlags {
	sf := scanFlags{
		kubeconfig:    fs.String("kubeconfig", "", "path to the kubeconfig (default: $KUBECONFIG, in-cluster, ~/.kube/config)"),
		namespaces:    fs.String("namespace", "", "only these namespaces (exact name, comma-separated for multiple)"),
		labelSelector: fs.String("label-selector", "", "label selector for namespaces, e.g. 'team=platform'"),
		nsRegex:       fs.String("namespace-regex", "", "only namespaces whose name matches this regex"),
		excludeRegex:  fs.String("exclude-regex", "", "exclude namespaces, e.g. '^(kube-|openshift-)' for system namespaces"),
		warnFactor:    fs.Float64("warn-factor", 10, "overprovisioning factor at which a namespace/pod is classified as a hoarder warning"),
		critFactor:    fs.Float64("crit-factor", 50, "overprovisioning factor at which the classification becomes critical"),
		capFactor:     fs.Float64("cap-factor", 1000, "clamp for the factor when usage is near zero (instead of infinity)"),
		limitWarnPct:  fs.Float64("limit-warn", 90, "limit utilization in %% at which a pod/namespace counts as limit risk"),
		timeout:       fs.Duration("timeout", 2*time.Minute, "overall timeout"),
	}
	fs.StringVar(sf.namespaces, "n", "", "shorthand for --namespace")
	return sf
}

func (sf scanFlags) thresholds() (model.Thresholds, error) {
	th := model.Thresholds{
		WarnFactor:   *sf.warnFactor,
		CritFactor:   *sf.critFactor,
		CapFactor:    *sf.capFactor,
		LimitWarnPct: *sf.limitWarnPct,
	}
	if th.WarnFactor <= 1 || th.CritFactor < th.WarnFactor || th.CapFactor < th.CritFactor {
		return th, fmt.Errorf("implausible thresholds: expected 1 < warn-factor (%.1f) <= crit-factor (%.1f) <= cap-factor (%.1f)",
			th.WarnFactor, th.CritFactor, th.CapFactor)
	}
	return th, nil
}

func (sf scanFlags) options() (collector.Options, error) {
	opts := collector.Options{LabelSelector: *sf.labelSelector}
	for _, n := range strings.Split(*sf.namespaces, ",") {
		if n = strings.TrimSpace(n); n != "" {
			opts.Names = append(opts.Names, n)
		}
	}
	if *sf.nsRegex != "" {
		re, err := regexp.Compile(*sf.nsRegex)
		if err != nil {
			return opts, fmt.Errorf("invalid --namespace-regex: %w", err)
		}
		opts.IncludeRegex = re
	}
	if *sf.excludeRegex != "" {
		re, err := regexp.Compile(*sf.excludeRegex)
		if err != nil {
			return opts, fmt.Errorf("invalid --exclude-regex: %w", err)
		}
		opts.ExcludeRegex = re
	}
	return opts, nil
}

// selectionFlagsSet reports whether selection flags were used (conflict
// check for suggest --from, where the report determines the selection).
func (sf scanFlags) selectionFlagsSet() bool {
	return *sf.namespaces != "" || *sf.labelSelector != "" || *sf.nsRegex != "" || *sf.excludeRegex != ""
}

func buildClients(kubeconfig string) (*rest.Config, *kubernetes.Clientset, *metricsclient.Clientset, error) {
	cfg, err := collector.BuildRESTConfig(kubeconfig)
	if err != nil {
		return nil, nil, nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create kube client: %w", err)
	}
	metricsClient, err := metricsclient.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create metrics client: %w", err)
	}
	return cfg, kubeClient, metricsClient, nil
}

func openOutput(path string) (*os.File, func(), error) {
	if path == "" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create output file %q: %w", path, err)
	}
	return f, func() { f.Close() }, nil
}

func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	sf := registerScanFlags(fs)
	format := fs.String("format", "table", "output format: table | html | json")
	output := fs.String("output", "", "output file (default: stdout)")
	detail := fs.String("detail", "hoarders", "container detail view in the terminal: none | hoarders | all")
	resolveOwners := fs.Bool("resolve-owners", false, "include the owner workload per pod in the report (required for 'suggest --from', extra API calls)")
	failOn := fs.String("fail-on", "none", "exit code 3 on findings: none | warning | critical")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *failOn != "none" && *failOn != "warning" && *failOn != "critical" {
		return fmt.Errorf("unknown --fail-on value %q (allowed: none, warning, critical)", *failOn)
	}

	th, err := sf.thresholds()
	if err != nil {
		return err
	}
	opts, err := sf.options()
	if err != nil {
		return err
	}
	opts.ResolveOwners = *resolveOwners
	cfg, kubeClient, metricsClient, err := buildClients(*sf.kubeconfig)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *sf.timeout)
	defer cancel()

	rep, err := collector.New(kubeClient, metricsClient).Collect(ctx, opts, th, cfg.Host)
	if err != nil {
		return err
	}

	out, closeOut, err := openOutput(*output)
	if err != nil {
		return err
	}
	defer closeOut()

	switch *format {
	case "table":
		mode, err := report.ParseDetailMode(*detail)
		if err != nil {
			return err
		}
		err = report.WriteTerminal(out, rep, mode)
		if err != nil {
			return err
		}
	case "json":
		if err := report.WriteJSON(out, rep); err != nil {
			return err
		}
	case "html":
		if err := report.WriteHTML(out, rep); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown format %q (allowed: table, html, json)", *format)
	}
	return checkFailOn(*failOn, rep)
}

// checkFailOn returns errFindings when the --fail-on threshold is crossed
// (for cron/CI: exit 0 = clean, 3 = findings, 1 = technical error).
func checkFailOn(failOn string, rep *model.Report) error {
	s := rep.Summary
	switch failOn {
	case "critical":
		if s.HoardersCritical > 0 {
			return fmt.Errorf("%w: %d namespaces hoarding at critical level (--fail-on=critical)", errFindings, s.HoardersCritical)
		}
	case "warning":
		if s.HoardersCritical+s.HoardersWarning > 0 {
			return fmt.Errorf("%w: %d namespaces hoarding at warning level or above (--fail-on=warning)", errFindings, s.HoardersCritical+s.HoardersWarning)
		}
	}
	return nil
}

func runSuggest(args []string) error {
	fs := flag.NewFlagSet("suggest", flag.ExitOnError)
	sf := registerScanFlags(fs)
	headroom := fs.Float64("headroom", 2.0, "multiplier on the measured usage for the request suggestion (2.0 = 100%% buffer)")
	minFactor := fs.Float64("min-factor", 0, "only containers with an overprovisioning factor >= this value (default: --warn-factor, or the report's warn threshold)")
	output := fs.String("output", "", "plan file (default: stdout)")
	from := fs.String("from", "", "build the plan from an existing JSON report instead of rescanning (report must be created with 'scan --resolve-owners --format=json')")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *headroom < 1.0 {
		return fmt.Errorf("--headroom < 1.0 would set requests below the measured usage")
	}

	var rep *model.Report
	if *from != "" {
		if sf.selectionFlagsSet() {
			return fmt.Errorf("--from excludes -n/--namespace, --label-selector, --namespace-regex and --exclude-regex — the report determines the selection")
		}
		var err error
		rep, err = report.LoadJSON(*from)
		if err != nil {
			return err
		}
		if !reportHasOwners(rep) {
			return fmt.Errorf("report %q contains no owner information — regenerate it with 'kubehoard scan --resolve-owners --format=json' or use suggest without --from", *from)
		}
		if *minFactor == 0 {
			*minFactor = rep.Thresholds.WarnFactor
		}
		if *minFactor == 0 {
			return fmt.Errorf("the report contains no thresholds — please pass --min-factor")
		}
	} else {
		th, err := sf.thresholds()
		if err != nil {
			return err
		}
		if *minFactor == 0 {
			*minFactor = th.WarnFactor
		}
		opts, err := sf.options()
		if err != nil {
			return err
		}
		opts.ResolveOwners = true

		cfg, kubeClient, metricsClient, err := buildClients(*sf.kubeconfig)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(context.Background(), *sf.timeout)
		defer cancel()

		rep, err = collector.New(kubeClient, metricsClient).Collect(ctx, opts, th, cfg.Host)
		if err != nil {
			return err
		}
	}

	p := plan.BuildPlan(rep, *headroom, *minFactor)
	if len(p.Targets) == 0 && len(p.Skipped) == 0 {
		fmt.Fprintln(os.Stderr, "No containers above the threshold — no plan generated.")
		return nil
	}

	data, err := plan.Marshal(p)
	if err != nil {
		return err
	}
	out, closeOut, err := openOutput(*output)
	if err != nil {
		return err
	}
	defer closeOut()
	if _, err := out.Write(data); err != nil {
		return err
	}
	if *output != "" {
		fmt.Fprintf(os.Stderr, "Plan with %d target(s) written to %s (%d skipped).\n",
			len(p.Targets), *output, len(p.Skipped))
		fmt.Fprintf(os.Stderr, "Review/adjust the suggestions, then: kubehoard apply %s\n", *output)
	}
	return nil
}

// reportHasOwners checks whether at least one pod carries owner information
// (a prerequisite for building an applyable plan from a report).
func reportHasOwners(rep *model.Report) bool {
	for i := range rep.Namespaces {
		for j := range rep.Namespaces[i].Pods {
			if rep.Namespaces[i].Pods[j].OwnerKind != "" {
				return true
			}
		}
	}
	return false
}

func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to the kubeconfig (default: $KUBECONFIG, in-cluster, ~/.kube/config)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	dryRunOnly := fs.Bool("dry-run", false, "server-side dry-run only, change nothing")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kubehoard apply [flags] PLAN.yaml")
	}

	p, err := plan.Load(fs.Arg(0))
	if err != nil {
		return err
	}

	// Full client-side validation before the first API call.
	for _, t := range p.Targets {
		if err := plan.Validate(t); err != nil {
			return err
		}
	}

	_, kubeClient, _, err := buildClients(*kubeconfig)
	if err != nil {
		return err
	}
	applier := plan.NewApplier(kubeClient)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Printf("Planned changes (%d):\n", len(p.Targets))
	for _, t := range p.Targets {
		fmt.Printf("  %-11s %s/%s  container=%s  %s\n",
			t.Kind, t.Namespace, t.Workload, t.Container, describeChanges(t))
	}

	// Server-side dry-run for all targets: checks existence, RBAC,
	// LimitRanges/quotas and admission webhooks without changing anything.
	fmt.Println("\nServer-side dry-run ...")
	var dryErrs []string
	for _, t := range p.Targets {
		if err := applier.Apply(ctx, t, true); err != nil {
			dryErrs = append(dryErrs, err.Error())
		}
	}
	if len(dryErrs) > 0 {
		for _, e := range dryErrs {
			fmt.Fprintf(os.Stderr, "  ERROR: %s\n", e)
		}
		return fmt.Errorf("%d of %d targets failed the dry-run — nothing was changed", len(dryErrs), len(p.Targets))
	}
	fmt.Println("Dry-run ok — all targets are applyable.")

	if *dryRunOnly {
		return nil
	}

	if !*yes {
		fmt.Print("\nApply the changes? The workloads will perform a rolling restart afterwards. [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted — nothing was changed.")
			return nil
		}
	}

	failed := 0
	for _, t := range p.Targets {
		if err := applier.Apply(ctx, t, false); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  ERROR: %v\n", err)
			continue
		}
		fmt.Printf("  patched: %-11s %s/%s  container=%s\n", t.Kind, t.Namespace, t.Workload, t.Container)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d patches failed", failed, len(p.Targets))
	}
	fmt.Println("\nDone. Rollouts are starting now; track progress e.g. with:")
	fmt.Println("  kubectl rollout status deployment/<name> -n <namespace>")
	return nil
}

func describeChanges(t plan.ContainerTarget) string {
	var parts []string
	if t.CPURequest != "" {
		parts = append(parts, "cpu.request="+t.CPURequest)
	}
	if t.MemRequest != "" {
		parts = append(parts, "mem.request="+t.MemRequest)
	}
	if t.CPULimit != "" {
		parts = append(parts, "cpu.limit="+t.CPULimit)
	}
	if t.MemLimit != "" {
		parts = append(parts, "mem.limit="+t.MemLimit)
	}
	return strings.Join(parts, " ")
}
