
function setupSignalHandlers() {
    // Graceful shutdown
    process.on('SIGTERM', async () => {
        console.log('[Master] Received SIGTERM, shutting down...');
        await stopWorker(true);
        process.exit(0);
    });

    process.on('SIGINT', async () => {
        console.log('[Master] Received SIGINT, shutting down...');
        await stopWorker(true);
        process.exit(0);
    });

    // Uncaught exceptions
    process.on('uncaughtException', (error) => {
        console.error('[Master] Uncaught exception:', error);
    });

    process.on('unhandledRejection', (reason, promise) => {
        console.error('[Master] Unhandled rejection at:', promise, 'reason:', reason);
    });
}

/**
 * Main function
 */
async function main() {
    console.log('='.repeat(50));
    console.log('[Master] Armorcode Proxy AI API Master Process');
    console.log('[Master] PID:', process.pid);
    console.log('[Master] Node version:', process.version);
    console.log('[Master] Working directory:', process.cwd());
    console.log('='.repeat(50));

    // Set up signal handlers
    setupSignalHandlers();

    // Create management server
    createMasterServer();

    // Start child process
    startWorker();
}

// Start master process
main().catch(error => {
    console.error('[Master] Failed to start:', error);
    process.exit(1);
});