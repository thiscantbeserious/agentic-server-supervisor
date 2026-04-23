use clap::{Parser, Subcommand};
use std::process::Command;

#[derive(Parser)]
#[command(name = "sentinel-cli")]
#[command(about = "Opinionated Management CLI for the Agentic Server Supervisor", long_about = None)]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Check the status of the Server Supervisor service
    Status,
    /// Restart the Supervisor service to apply configuration changes
    Restart,
    /// Tail the Supervisor service logs
    Logs,
}

fn main() {
    let cli = Cli::parse();

    match &cli.command {
        Commands::Status => {
            run_zeroclaw("service status");
        }
        Commands::Restart => {
            run_zeroclaw("service restart");
        }
        Commands::Logs => {
            run_zeroclaw("service logs");
        }
    }
}

fn run_zeroclaw(sub_cmd: &str) {
    let mut cmd = Command::new("zeroclaw");
    let current_dir = std::env::current_dir().expect("Failed to get current directory");
    
    cmd.arg("--config-dir")
       .arg(current_dir);

    for arg in sub_cmd.split_whitespace() {
        cmd.arg(arg);
    }

    let status = cmd.status().expect("Failed to execute zeroclaw command");
    
    if !status.success() {
        std::process::exit(status.code().unwrap_or(1));
    }
}
