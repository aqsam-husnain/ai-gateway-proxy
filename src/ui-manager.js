import { existsSync, readFileSync, writeFileSync } from 'fs';
import { promises as fs } from 'fs';
import path from 'path';
import os from 'os';
import multer from 'multer';
import crypto from 'crypto';
import AdmZip from 'adm-zip';
import { getRequestBody } from './common.js';

// CPU usage calculation related variables
let previousCpuInfo = null;


async function handleLoginRequest(req, res) {
    if (req.method !== 'POST') {
        res.writeHead(405, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ success: false, message: 'Only POST requests are supported' }));
        return true;
    }

    try {
        const requestData = await parseRequestBody(req);
        const { password } = requestData;
        
        if (!password) {
            res.writeHead(400, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ success: false, message: 'Password cannot be empty' }));
            return true;
        }

        const isValid = await validateCredentials(password);
        
        if (isValid) {
            // Generate simple token
            const token = generateToken();
            const expiryTime = getExpiryTime();
            
            // Store token info to local file
            await saveToken(token, {
                username: 'admin',
                loginTime: Date.now(),
                expiryTime
            });

            res.writeHead(200, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({
                success: true,
                message: 'Login successful',
                token,
                expiresIn: '24 hours'
            }));
        } else {
            res.writeHead(401, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({
                success: false,
                message: 'Incorrect password, please try again'
            }));
        }
    } catch (error) {
        console.error('[Auth] Login processing error:', error);
        res.writeHead(500, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({
            success: false,
            message: error.message || 'Server error'
        }));
    }
    return true;
}

// Configure multer middleware
const storage = multer.diskStorage({
    destination: async (req, file, cb) => {
        try {
            // When multer destination callback is called, req.body is not yet parsed, use default path first
            // The actual provider will be obtained from req.body after file upload completes
            const uploadPath = path.join(process.cwd(), 'configs', 'temp');
            await fs.mkdir(uploadPath, { recursive: true });
            cb(null, uploadPath);
        } catch (error) {
            cb(error);
        }
    },
    filename: (req, file, cb) => {
        const timestamp = Date.now();
        const sanitizedName = file.originalname.replace(/[^a-zA-Z0-9.-]/g, '_');
        cb(null, `${timestamp}_${sanitizedName}`);
    }
});

const fileFilter = (req, file, cb) => {
    const allowedTypes = ['.json', '.txt', '.key', '.pem', '.p12', '.pfx'];
    const ext = path.extname(file.originalname).toLowerCase();
    if (allowedTypes.includes(ext)) {
        cb(null, true);
    } else {
        cb(new Error('Unsupported file type'), false);
    }
};

const upload = multer({
    storage,
    fileFilter,
    limits: {
        fileSize: 5 * 1024 * 1024 // 5MB limit
    }
});


export function initializeUIManagement() {
    // Initialize log broadcasting for UI
    if (!global.eventClients) {
        global.eventClients = [];
    }
    if (!global.logBuffer) {
        global.logBuffer = [];
    }

    // Override console.log to broadcast logs
    const originalLog = console.log;
    console.log = function(...args) {
        originalLog.apply(console, args);
        const message = args.map(arg => {
            if (typeof arg === 'string') return arg;
            try {
                return JSON.stringify(arg);
            } catch (e) {
                if (arg instanceof Error) {
                    return `[Error: ${arg.message}] ${arg.stack || ''}`;
                }
                return `[Object: ${Object.prototype.toString.call(arg)}] (Circular or too complex to stringify)`;
            }
        }).join(' ');
        const logEntry = {
            timestamp: new Date().toISOString(),
            level: 'info',
            message: message
        };
        global.logBuffer.push(logEntry);
        if (global.logBuffer.length > 100) {
            global.logBuffer.shift();
        }
        broadcastEvent('log', logEntry);
    };

    // Override console.error to broadcast errors
    const originalError = console.error;
    console.error = function(...args) {
        originalError.apply(console, args);
        const message = args.map(arg => {
            if (typeof arg === 'string') return arg;
            try {
                return JSON.stringify(arg);
            } catch (e) {
                if (arg instanceof Error) {
                    return `[Error: ${arg.message}] ${arg.stack || ''}`;
                }
                return `[Object: ${Object.prototype.toString.call(arg)}] (Circular or too complex to stringify)`;
            }
        }).join(' ');
        const logEntry = {
            timestamp: new Date().toISOString(),
            level: 'error',
            message: message
        };
        global.logBuffer.push(logEntry);
        if (global.logBuffer.length > 100) {
            global.logBuffer.shift();
        }
        broadcastEvent('log', logEntry);
    };
}

/**
 * Helper function to broadcast events to UI clients
 * @param {string} eventType - The type of event
 * @param {any} data - The data to broadcast
 */
export function broadcastEvent(eventType, data) {
    if (global.eventClients && global.eventClients.length > 0) {
        const payload = typeof data === 'string' ? data : JSON.stringify(data);
        global.eventClients.forEach(client => {
            client.write(`event: ${eventType}\n`);
            client.write(`data: ${payload}\n\n`);
        });
    }
}

/**
 * Scan and analyze configuration files
 * @param {Object} currentConfig - The current configuration object
 * @param {Object} providerPoolManager - Provider pool manager instance
 * @returns {Promise<Array>} Array of configuration file objects
 */
async function scanConfigFiles(currentConfig, providerPoolManager) {
    const configFiles = [];
    
    // Only scan configs directory
    const configsPath = path.join(process.cwd(), 'configs');
    
    if (!existsSync(configsPath)) {
        // console.log('[Config Scanner] configs directory not found, creating empty result');
        return configFiles;
    }

    const usedPaths = new Set(); // Store used paths for determining association status

    // Extract all OAuth credentials file paths from config - normalize path format
    addToUsedPaths(usedPaths, currentConfig.GEMINI_OAUTH_CREDS_FILE_PATH);

    // Use latest provider pool data
    let providerPools = currentConfig.providerPools;
    if (providerPoolManager && providerPoolManager.providerPools) {
        providerPools = providerPoolManager.providerPools;
    }

    // Check all OAuth credentials paths in provider pools file - normalize path format
    if (providerPools) {
        for (const [providerType, providers] of Object.entries(providerPools)) {
            for (const provider of providers) {
                addToUsedPaths(usedPaths, provider.GEMINI_OAUTH_CREDS_FILE_PATH);
                addToUsedPaths(usedPaths, provider.ANTIGRAVITY_OAUTH_CREDS_FILE_PATH);
            }
        }
    }

    try {
        // Scan all subdirectories and files under configs directory
        const configsFiles = await scanOAuthDirectory(configsPath, usedPaths, currentConfig);
        configFiles.push(...configsFiles);
    } catch (error) {
        console.warn(`[Config Scanner] Failed to scan configs directory:`, error.message);
    }

    return configFiles;
}

/**
 * Analyze OAuth configuration file and return metadata
 * @param {string} filePath - Full path to the file
 * @param {Set} usedPaths - Set of paths currently in use
 * @returns {Promise<Object|null>} OAuth file information object
 */
async function analyzeOAuthFile(filePath, usedPaths, currentConfig) {
    try {
        const stats = await fs.stat(filePath);
        const ext = path.extname(filePath).toLowerCase();
        const filename = path.basename(filePath);
        const relativePath = path.relative(process.cwd(), filePath);
        
        // Read file content for analysis
        let content = '';
        let type = 'oauth_credentials';
        let isValid = true;
        let errorMessage = '';
        let oauthProvider = 'unknown';
        let usageInfo = getFileUsageInfo(relativePath, filename, usedPaths, currentConfig);
        
        try {
            if (ext === '.json') {
                const rawContent = await fs.readFile(filePath, 'utf8');
                const jsonData = JSON.parse(rawContent);
                content = rawContent;
                
                // Identify OAuth provider
                if (jsonData.apiKey || jsonData.api_key) {
                    type = 'api_key';
                } else if (jsonData.client_id || jsonData.client_secret) {
                    oauthProvider = 'oauth2';
                } else if (jsonData.access_token || jsonData.refresh_token) {
                    oauthProvider = 'token_based';
                } else if (jsonData.credentials) {
                    oauthProvider = 'service_account';
                }
                
                if (jsonData.base_url || jsonData.endpoint) {
                    if (jsonData.base_url.includes('openai.com')) {
                        oauthProvider = 'openai';
                    } else if (jsonData.base_url.includes('anthropic.com')) {
                        oauthProvider = 'claude';
                    } else if (jsonData.base_url.includes('googleapis.com')) {
                        oauthProvider = 'gemini';
                    }
                }
            } else {
                content = await fs.readFile(filePath, 'utf8');
                
                if (ext === '.key' || ext === '.pem') {
                    if (content.includes('-----BEGIN') && content.includes('PRIVATE KEY-----')) {
                        oauthProvider = 'private_key';
                    }
                } else if (ext === '.txt') {
                    if (content.includes('api_key') || content.includes('apikey')) {
                        oauthProvider = 'api_key';
                    }
                } else if (ext === '.oauth' || ext === '.creds') {
                    oauthProvider = 'oauth_credentials';
                }
            }
        } catch (readError) {
            isValid = false;
            errorMessage = `Unable to read file: ${readError.message}`;
        }
        
        return {
            name: filename,
            path: relativePath,
            size: stats.size,
            type: type,
            provider: oauthProvider,
            extension: ext,
            modified: stats.mtime.toISOString(),
            isValid: isValid,
            errorMessage: errorMessage,
            isUsed: isPathUsed(relativePath, filename, usedPaths),
            usageInfo: usageInfo, // Added detailed association info
            preview: content.substring(0, 100) + (content.length > 100 ? '...' : '')
        };
    } catch (error) {
        console.warn(`[OAuth Analyzer] Failed to analyze file ${filePath}:`, error.message);
        return null;
    }
}

/**
 * Get detailed usage information for a file
 * @param {string} relativePath - Relative file path
 * @param {string} fileName - File name
 * @param {Set} usedPaths - Set of used paths
 * @param {Object} currentConfig - Current configuration
 * @returns {Object} Usage information object
 */
function getFileUsageInfo(relativePath, fileName, usedPaths, currentConfig) {
    const usageInfo = {
        isUsed: false,
        usageType: null,
        usageDetails: []
    };

    // Check if being used
    const isUsed = isPathUsed(relativePath, fileName, usedPaths);
    if (!isUsed) {
        return usageInfo;
    }

    usageInfo.isUsed = true;

    // Check usage in main config
    if (currentConfig.GEMINI_OAUTH_CREDS_FILE_PATH &&
        (pathsEqual(relativePath, currentConfig.GEMINI_OAUTH_CREDS_FILE_PATH) ||
         pathsEqual(relativePath, currentConfig.GEMINI_OAUTH_CREDS_FILE_PATH.replace(/\\/g, '/')))) {
        usageInfo.usageType = 'main_config';
        usageInfo.usageDetails.push({
            type: 'Main Config',
            location: 'Gemini OAuth credentials file path',
            configKey: 'GEMINI_OAUTH_CREDS_FILE_PATH'
        });
    }

    // Check usage in provider pools
    if (currentConfig.providerPools) {
        // Use flatMap to optimize double loop to single loop O(n)
        const allProviders = Object.entries(currentConfig.providerPools).flatMap(
            ([providerType, providers]) =>
                providers.map((provider, index) => ({ provider, providerType, index }))
        );

        for (const { provider, providerType, index } of allProviders) {
            const providerUsages = [];

            if (provider.GEMINI_OAUTH_CREDS_FILE_PATH &&
                (pathsEqual(relativePath, provider.GEMINI_OAUTH_CREDS_FILE_PATH) ||
                 pathsEqual(relativePath, provider.GEMINI_OAUTH_CREDS_FILE_PATH.replace(/\\/g, '/')))) {
                providerUsages.push({
                    type: 'Provider Pool',
                    location: `Gemini OAuth credentials (node ${index + 1})`,
                    providerType: providerType,
                    providerIndex: index,
                    configKey: 'GEMINI_OAUTH_CREDS_FILE_PATH'
                });
            }

            if (provider.ANTIGRAVITY_OAUTH_CREDS_FILE_PATH &&
                (pathsEqual(relativePath, provider.ANTIGRAVITY_OAUTH_CREDS_FILE_PATH) ||
                 pathsEqual(relativePath, provider.ANTIGRAVITY_OAUTH_CREDS_FILE_PATH.replace(/\\/g, '/')))) {
                providerUsages.push({
                    type: 'Provider Pool',
                    location: `Antigravity OAuth credentials (node ${index + 1})`,
                    providerType: providerType,
                    providerIndex: index,
                    configKey: 'ANTIGRAVITY_OAUTH_CREDS_FILE_PATH'
                });
            }
            
            if (providerUsages.length > 0) {
                usageInfo.usageType = 'provider_pool';
                usageInfo.usageDetails.push(...providerUsages);
            }
        }
    }

    // If there are multiple usage locations, mark as multiple purposes
    if (usageInfo.usageDetails.length > 1) {
        usageInfo.usageType = 'multiple';
    }

    return usageInfo;
}

/**
 * Scan OAuth directory for credential files
 * @param {string} dirPath - Directory path to scan
 * @param {Set} usedPaths - Set of used paths
 * @param {Object} currentConfig - Current configuration
 * @returns {Promise<Array>} Array of OAuth configuration file objects
 */
async function scanOAuthDirectory(dirPath, usedPaths, currentConfig) {
    const oauthFiles = [];
    
    try {
        const files = await fs.readdir(dirPath, { withFileTypes: true });
        
        for (const file of files) {
            const fullPath = path.join(dirPath, file.name);
            
            if (file.isFile()) {
                const ext = path.extname(file.name).toLowerCase();
                // Only focus on OAuth-related file types
                if (['.json', '.oauth', '.creds', '.key', '.pem', '.txt'].includes(ext)) {
                    const fileInfo = await analyzeOAuthFile(fullPath, usedPaths, currentConfig);
                    if (fileInfo) {
                        oauthFiles.push(fileInfo);
                    }
                }
            } else if (file.isDirectory()) {
                // Recursively scan subdirectories (limit depth)
                const relativePath = path.relative(process.cwd(), fullPath);
                // Max depth 4 levels, to support structures like configs/gemini/{subfolder}/file.json
                if (relativePath.split(path.sep).length < 4) {
                    const subFiles = await scanOAuthDirectory(fullPath, usedPaths, currentConfig);
                    oauthFiles.push(...subFiles);
                }
            }
        }
    } catch (error) {
        console.warn(`[OAuth Scanner] Failed to scan directory ${dirPath}:`, error.message);
    }
    
    return oauthFiles;
}


// Note: normalizePath, getFileName, pathsEqual, isPathUsed, detectProviderFromPath
// have been moved to provider-utils.js common module

/**
 * Get usage info for all providers that support usage queries
 * @param {Object} currentConfig - Current configuration
 * @param {Object} providerPoolManager - Provider pool manager
 * @returns {Promise<Object>} Usage info for all providers
 */
async function getAllProvidersUsage(currentConfig, providerPoolManager) {
    const results = {
        timestamp: new Date().toISOString(),
        providers: {}
    };

    // List of providers that support usage queries
    const supportedProviders = ['gemini-cli-oauth', 'gemini-antigravity'];

    // Concurrently get usage data for all providers
    const usagePromises = supportedProviders.map(async (providerType) => {
        try {
            const providerUsage = await getProviderTypeUsage(providerType, currentConfig, providerPoolManager);
            return { providerType, data: providerUsage, success: true };
        } catch (error) {
            return {
                providerType,
                data: {
                    error: error.message,
                    instances: []
                },
                success: false
            };
        }
    });

    // Wait for all concurrent requests to complete
    const usageResults = await Promise.all(usagePromises);

    // Integrate results into results.providers
    for (const result of usageResults) {
        results.providers[result.providerType] = result.data;
    }

    return results;
}

/**
 * Get usage info for specified provider type
 * @param {string} providerType - Provider type
 * @param {Object} currentConfig - Current configuration
 * @param {Object} providerPoolManager - Provider pool manager
 * @returns {Promise<Object>} Provider usage info
 */
async function getProviderTypeUsage(providerType, currentConfig, providerPoolManager) {
    const result = {
        providerType,
        instances: [],
        totalCount: 0,
        successCount: 0,
        errorCount: 0
    };

    // Get all instances from provider pool
    let providers = [];
    if (providerPoolManager && providerPoolManager.providerPools && providerPoolManager.providerPools[providerType]) {
        providers = providerPoolManager.providerPools[providerType];
    } else if (currentConfig.providerPools && currentConfig.providerPools[providerType]) {
        providers = currentConfig.providerPools[providerType];
    }

    result.totalCount = providers.length;

    // Iterate through all provider instances to get usage
    for (const provider of providers) {
        const providerKey = providerType + (provider.uuid || '');
        let adapter = serviceInstances[providerKey];
        
        const instanceResult = {
            uuid: provider.uuid || 'unknown',
            name: getProviderDisplayName(provider, providerType),
            isHealthy: provider.isHealthy !== false,
            isDisabled: provider.isDisabled === true,
            success: false,
            usage: null,
            error: null
        };

        // First check if disabled, skip initialization for disabled providers
        if (provider.isDisabled) {
            instanceResult.error = 'Provider is disabled';
            result.errorCount++;
        } else if (!adapter) {
            // Service instance not initialized, try auto-initialization
            try {
                console.log(`[Usage API] Auto-initializing service adapter for ${providerType}: ${provider.uuid}`);
                // Build configuration object
                const serviceConfig = {
                    ...CONFIG,
                    ...provider,
                    MODEL_PROVIDER: providerType
                };
                adapter = getServiceAdapter(serviceConfig);
            } catch (initError) {
                console.error(`[Usage API] Failed to initialize adapter for ${providerType}: ${provider.uuid}:`, initError.message);
                instanceResult.error = `Service instance initialization failed: ${initError.message}`;
                result.errorCount++;
            }
        }
        
        // If adapter exists (including just initialized), and no error, try to get usage
        if (adapter && !instanceResult.error) {
            try {
                const usage = await getAdapterUsage(adapter, providerType);
                instanceResult.success = true;
                instanceResult.usage = usage;
                result.successCount++;
            } catch (error) {
                instanceResult.error = error.message;
                result.errorCount++;
            }
        }

        result.instances.push(instanceResult);
    }

    return result;
}

/**
 * Get usage info from adapter
 * @param {Object} adapter - Service adapter
 * @param {string} providerType - Provider type
 * @returns {Promise<Object>} Usage info
 */
async function getAdapterUsage(adapter, providerType) {
    if (providerType === 'gemini-cli-oauth') {
        if (typeof adapter.getUsageLimits === 'function') {
            const rawUsage = await adapter.getUsageLimits();
            return formatGeminiUsage(rawUsage);
        } else if (adapter.geminiApiService && typeof adapter.geminiApiService.getUsageLimits === 'function') {
            const rawUsage = await adapter.geminiApiService.getUsageLimits();
            return formatGeminiUsage(rawUsage);
        }
        throw new Error('This adapter does not support usage query');
    }
    
    if (providerType === 'gemini-antigravity') {
        if (typeof adapter.getUsageLimits === 'function') {
            const rawUsage = await adapter.getUsageLimits();
            return formatAntigravityUsage(rawUsage);
        } else if (adapter.antigravityApiService && typeof adapter.antigravityApiService.getUsageLimits === 'function') {
            const rawUsage = await adapter.antigravityApiService.getUsageLimits();
            return formatAntigravityUsage(rawUsage);
        }
        throw new Error('This adapter does not support usage query');
    }
    
    throw new Error(`Unsupported provider type: ${providerType}`);
}

/**
 * Get provider display name
 * @param {Object} provider - Provider configuration
 * @param {string} providerType - Provider type
 * @returns {string} Display name
 */
function getProviderDisplayName(provider, providerType) {
    // Claude Code uses customName directly
    if (providerType === 'claudeCode-custom') {
        return provider.customName || provider.uuid || 'Claude Code CLI';
    }

    // Try to extract name from credentials file path
    const credPathKey = {
        'gemini-cli-oauth': 'GEMINI_OAUTH_CREDS_FILE_PATH',
        'gemini-antigravity': 'ANTIGRAVITY_OAUTH_CREDS_FILE_PATH'
    }[providerType];

    if (credPathKey && provider[credPathKey]) {
        const filePath = provider[credPathKey];
        const fileName = path.basename(filePath);
        const dirName = path.basename(path.dirname(filePath));
        return `${dirName}/${fileName}`;
    }

    return provider.uuid || 'Unnamed';
}
