#pragma once
#include <string>
#include <unordered_map>
#include <list>
#include <atomic>
#include "logger/Logger.h"
#include <MetricConstants.h>


namespace logtail {


class Counter{

    public:
        Counter();
        Counter(std::string name);
        ~Counter();

        std::string mName;
        std::atomic_long mVal;
        std::atomic_long mTimestamp;
        void Add(uint64_t val);
        void Set(uint64_t val);
        Counter* Copy();
};

class Metrics {
    public:
        Metrics(std::vector<std::pair<std::string, std::string> > labels);
        Metrics();
        ~Metrics();
        
        std::atomic_bool mDeleted;
        std::vector<std::pair<std::string, std::string>> mLabels;
        std::vector<Counter*> mValues;
        Counter* CreateCounter(std::string Name);
        Metrics* Copy();
        Metrics* next = NULL;
};

class WriteMetrics {
    private:
        WriteMetrics();
        ~WriteMetrics();
    public:
        static WriteMetrics* GetInstance() {
            static WriteMetrics* ptr = new WriteMetrics();
            return ptr;
        }
        Metrics* CreateMetrics(std::vector<std::pair<std::string, std::string>> Labels);
        void DestroyMetrics(Metrics* metrics);
        void ClearDeleted();
        Metrics* DoSnapshot();
        // empty head node
        Metrics* mHead = new Metrics();
        Metrics* mTail = mHead;
        std::vector<Metrics*> mDeletedMetrics;
        std::mutex mMutex;
};

class ReadMetrics {
   private:
        ReadMetrics();
        ~ReadMetrics();
    public:
        static ReadMetrics* GetInstance() {
            static ReadMetrics* ptr = new ReadMetrics();
            return ptr;
        }
        void Read();
        void UpdateMetrics();
        Metrics* mHead = NULL;
        std::mutex mMutex;
        WriteMetrics* mWriteMetrics;
};
}