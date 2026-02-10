// Authentication module - handles token management and API call encapsulation

class ApiClient {
    constructor() {
        this.authManager = new AuthManager();
        this.baseURL = window.location.origin;
    }

    
    handleUnauthorized() {
        this.authManager.clearToken();
        window.location.href = '/login.html';
    }

    
    async get(endpoint, params = {}) {
        const queryString = new URLSearchParams(params).toString();
        const url = queryString ? `${endpoint}?${queryString}` : endpoint;
        return this.request(url, { method: 'GET' });
    }

    
async function initAuth() {
    const authManager = new AuthManager();
    
    // Check if there is already a valid token
    if (authManager.isTokenValid()) {
        // Verify token is still valid (send a test request)
        try {
            const apiClient = new ApiClient();
            await apiClient.get('/health');
            return true;
        } catch (error) {
            // Token invalid, clear and redirect to login page
            authManager.clearToken();
            window.location.href = '/login.html';
            return false;
        }
    } else {
        // No valid token, redirect to login page
        window.location.href = '/login.html';
        return false;
    }
}

/**
 * Logout function
 */
async function logout() {
    const authManager = new AuthManager();
    await authManager.logout();
}

/**
 * Login function (for login page use)
 */
async function login(password, rememberMe = false) {
    try {
        const response = await fetch('/api/login', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json'
            },
            body: JSON.stringify({
            password,
            rememberMe
            })
        });

        const data = await response.json();

        if (data.success) {
            // Save token
            const authManager = new AuthManager();
            authManager.saveToken(data.token, rememberMe);
            return { success: true };
        } else {
            return { success: false, message: data.message };
        }
    } catch (error) {
        console.error('Login error:', error);
        return { success: false, message: 'Login failed, please check network connection' };
    }
}

// Create singleton instances
const authManager = new AuthManager();
const apiClient = new ApiClient();

/**
 * Get request headers with authentication (convenience function)
 * @returns {Object} Request headers containing authentication info
 */
function getAuthHeaders() {
    return apiClient.getAuthHeaders();
}

// Export instances to window (for legacy code compatibility)
window.authManager = authManager;
window.apiClient = apiClient;
window.initAuth = initAuth;
window.logout = logout;
window.login = login;

// Export AuthManager class and ApiClient class for other modules to use
window.AuthManager = AuthManager;
window.ApiClient = ApiClient;

// ES6 module exports
export {
    AuthManager,
    ApiClient,
    authManager,
    apiClient,
    initAuth,
    logout,
    login,
    getAuthHeaders
};

console.log('Authentication module loaded');