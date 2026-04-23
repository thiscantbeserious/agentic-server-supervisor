use clap::{Parser, Subcommand};
use std::process::Command;

#[derive(Parser)]
#[command(name = "sentinel")]
#[command(about = "Agentic Server Supervisor Management CLI", long_about = None)]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Check the status of the Server Sentinel
    Status,
    /// Restart the Server Sentinel service
    Restart,
    /// View Sentinel service logs
    Logs,
    /// Manage mesh communication (Future)
    Mesh {
        #[arg(short, long)]
        connect: Option<String>,
    },
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
        Commands::Mesh { connect } => {
            if let Some(peer) = connect {
                println!("🚀 Connecting to mesh peer: {}", peer);
                println!("ℹ️ Mesh channel plugin implementation pending...");
            } else {
                println!("🌐 Sentinel Mesh: Active (Scanning for peers)");
            }
        }
    }
}

fn run_zeroclaw(sub_cmd: &str) {
    let mut cmd = Command::new("zeroclaw");
    
    // Get the current directory to use as config-dir
    let current_dir = std::env::current_dir().expect("Failed to get current directory");
    
    // We expect the user to run this from the project root
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
